package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	robotsparser "github.com/igor-zatochniy/seo-auditor/internal/robots"
)

type requestStartRecorder struct {
	base   http.RoundTripper
	starts chan<- time.Time
}

func (r requestStartRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.starts <- time.Now()
	return r.base.RoundTrip(req)
}

func TestRobotsPolicyCacheEnforcesMemoryBudget(t *testing.T) {
	compilePolicy := func(t *testing.T, prefix string, ruleCount int) robotsPolicy {
		t.Helper()

		var content strings.Builder
		content.WriteString("User-agent: *\n")
		for index := range ruleCount {
			_, _ = fmt.Fprintf(&content, "Disallow: /%s/%d\n", prefix, index)
		}
		compiled, err := robotsparser.CompilePolicyContext(
			context.Background(),
			content.String(),
			UserAgentStr,
		)
		if err != nil {
			t.Fatalf("compile test robots.txt policy: %v", err)
		}
		return robotsPolicy{compiled: compiled}
	}

	t.Run("accepts more than 1024 rules within the byte and cache budgets", func(t *testing.T) {
		var content strings.Builder
		content.WriteString("User-agent: *\n")
		for index := range 2048 {
			_, _ = fmt.Fprintf(&content, "Disallow: /private/%d\n", index)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(content.String()))
		}))
		defer server.Close()

		cache := newRobotsPolicyCacheWithLimits(time.Hour, 16, 4*1024*1024, 1)
		allowed, err := cache.isAllowedByRobots(
			context.Background(),
			newRobotsHTTPClient(server.Client().Transport),
			server.URL+"/private/1",
			time.Second,
		)
		if err != nil {
			t.Fatalf("byte-bounded robots.txt was rejected: %v", err)
		}
		if allowed {
			t.Fatal("policy lost a Disallow rule after the former 1024-rule boundary")
		}
	})

	t.Run("rejects a policy larger than the cache budget", func(t *testing.T) {
		policy := compilePolicy(t, "oversized", 64)
		budget := policy.estimatedMemoryBytes() - 1
		cache := newRobotsPolicyCacheWithLimits(time.Hour, 16, budget, 1)

		cached, err := cache.policy(context.Background(), "https://oversized.example", func() (robotsPolicy, error) {
			return policy, nil
		})
		if err == nil {
			t.Fatal("oversized compiled policy unexpectedly entered the cache")
		}
		target, parseErr := url.Parse("https://oversized.example/private")
		if parseErr != nil {
			t.Fatalf("parse target URL: %v", parseErr)
		}
		if cached.allows(target) {
			t.Fatal("oversized compiled policy must fail closed")
		}
	})

	t.Run("evicts least recently used policies by total weight", func(t *testing.T) {
		first := compilePolicy(t, "first", 64)
		second := compilePolicy(t, "second", 64)
		budget := first.estimatedMemoryBytes() + second.estimatedMemoryBytes() - 1
		cache := newRobotsPolicyCacheWithLimits(time.Hour, 16, budget, 1)

		if _, err := cache.policy(context.Background(), "https://first.example", func() (robotsPolicy, error) {
			return first, nil
		}); err != nil {
			t.Fatalf("cache first policy: %v", err)
		}
		if _, err := cache.policy(context.Background(), "https://second.example", func() (robotsPolicy, error) {
			return second, nil
		}); err != nil {
			t.Fatalf("cache second policy: %v", err)
		}

		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.weight > cache.maxWeight {
			t.Fatalf("cache weight = %d, budget = %d", cache.weight, cache.maxWeight)
		}
		if _, exists := cache.entries["https://first.example"]; exists {
			t.Fatal("least recently used policy was not evicted")
		}
		if _, exists := cache.entries["https://second.example"]; !exists {
			t.Fatal("new policy was unexpectedly evicted")
		}
	})
}

func TestRobotsCacheSharesPolicyAcrossPaths(t *testing.T) {
	var robotsRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		robotsRequests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	}))
	defer server.Close()

	cache := newRobotsPolicyCache(time.Hour, 16)
	client := newRobotsHTTPClient(server.Client().Transport)

	allowed, err := cache.isAllowedByRobots(context.Background(), client, server.URL+"/public", time.Second)
	if err != nil || !allowed {
		t.Fatalf("unexpected public path decision: allowed=%t error=%v", allowed, err)
	}
	allowed, err = cache.isAllowedByRobots(context.Background(), client, server.URL+"/private/page", time.Second)
	if err != nil || allowed {
		t.Fatalf("unexpected private path decision: allowed=%t error=%v", allowed, err)
	}
	if robotsRequests.Load() != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1", robotsRequests.Load())
	}

	parsedServerURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	key, err := robotsPolicyCacheKey(parsedServerURL)
	if err != nil {
		t.Fatalf("build robots cache key: %v", err)
	}
	entry := cache.getEntry(key)
	entry.mu.Lock()
	compiled := entry.policy.compiled
	entry.mu.Unlock()
	if compiled == nil {
		t.Fatal("robots cache did not retain a compiled policy")
	}

	allowed, err = cache.isAllowedByRobots(context.Background(), client, server.URL+"/another", time.Second)
	if err != nil || !allowed {
		t.Fatalf("unexpected cached path decision: allowed=%t error=%v", allowed, err)
	}
	entry.mu.Lock()
	reused := entry.policy.compiled
	entry.mu.Unlock()
	if reused != compiled {
		t.Fatal("robots cache recompiled an unexpired policy")
	}
}

func TestIDNAliasesShareHostPolicyAndRobotsCacheKeys(t *testing.T) {
	unicodeURL, err := url.Parse("https://bücher.de/page")
	if err != nil {
		t.Fatalf("parse Unicode URL: %v", err)
	}
	punycodeURL, err := url.Parse("https://xn--bcher-kva.de/page")
	if err != nil {
		t.Fatalf("parse Punycode URL: %v", err)
	}

	unicodeHostKey, err := hostPolicyKey(unicodeURL)
	if err != nil {
		t.Fatalf("build Unicode host policy key: %v", err)
	}
	punycodeHostKey, err := hostPolicyKey(punycodeURL)
	if err != nil {
		t.Fatalf("build Punycode host policy key: %v", err)
	}
	if unicodeHostKey != punycodeHostKey {
		t.Fatalf("host policy keys differ: %q and %q", unicodeHostKey, punycodeHostKey)
	}

	unicodeCacheKey, err := robotsPolicyCacheKey(unicodeURL)
	if err != nil {
		t.Fatalf("build Unicode robots cache key: %v", err)
	}
	punycodeCacheKey, err := robotsPolicyCacheKey(punycodeURL)
	if err != nil {
		t.Fatalf("build Punycode robots cache key: %v", err)
	}
	if unicodeCacheKey != punycodeCacheKey {
		t.Fatalf("robots cache keys differ: %q and %q", unicodeCacheKey, punycodeCacheKey)
	}
}

func TestRobotsCacheDoesNotReuseExpiredAllowAllAfterServerFailure(t *testing.T) {
	var robotsRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if robotsRequests.Add(1) == 1 {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cacheTTL := 20 * time.Millisecond
	cache := newRobotsPolicyCache(cacheTTL, 16)
	client := newRobotsHTTPClient(server.Client().Transport)

	allowed, err := cache.isAllowedByRobots(context.Background(), client, server.URL+"/page", time.Second)
	if err != nil || !allowed {
		t.Fatalf("404 robots.txt should allow crawling: allowed=%t error=%v", allowed, err)
	}

	time.Sleep(2 * cacheTTL)
	allowed, err = cache.isAllowedByRobots(context.Background(), client, server.URL+"/page", time.Second)
	if err == nil || allowed {
		t.Fatalf("503 refresh must fail closed: allowed=%t error=%v", allowed, err)
	}
	if robotsRequests.Load() != 2 {
		t.Fatalf("robots.txt requests = %d, want 2", robotsRequests.Load())
	}
}

func TestPoliteTransportLimitsConcurrencyPerHost(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		number := requests.Add(1)
		if number == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		if number == 2 {
			close(secondStarted)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		server.Close()
	})

	manager := newHostPolicyManager(time.Millisecond, 1, 16, 100*time.Millisecond)
	client := &http.Client{Transport: &politeRoundTripper{base: server.Client().Transport, policies: manager}}
	errors := make(chan error, 2)

	request := func() {
		resp, err := client.Get(server.URL)
		if err == nil {
			err = resp.Body.Close()
		}
		errors <- err
	}
	go request()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	go request()

	select {
	case <-secondStarted:
		t.Fatal("second request started before the first released its host slot")
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	for range 2 {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not complete")
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("unexpected request count: %d", requests.Load())
	}
}

func TestPoliteTransportHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := newHostPolicyManager(time.Millisecond, 1, 16, 100*time.Millisecond)
	client := &http.Client{Transport: &politeRoundTripper{base: server.Client().Transport, policies: manager}}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()

	started := time.Now()
	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_ = resp.Body.Close()
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
		t.Fatalf("Retry-After was not honored: elapsed=%s", elapsed)
	}
}

func TestPoliteTransportAppliesRateLimitPerHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	requestStarts := make(chan time.Time, 2)
	manager := newHostPolicyManager(50*time.Millisecond, 1, 16, time.Second)
	client := &http.Client{Transport: &politeRoundTripper{
		base: requestStartRecorder{
			base:   server.Client().Transport,
			starts: requestStarts,
		},
		policies: manager,
	}}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()
	firstStarted := <-requestStarts

	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_ = resp.Body.Close()
	secondStarted := <-requestStarts
	if spacing := secondStarted.Sub(firstStarted); spacing < 35*time.Millisecond {
		t.Fatalf("per-host rate limit was not applied: spacing=%s", spacing)
	}
}

func TestWorkersShareRobotsCacheAndHostConcurrency(t *testing.T) {
	var robotsRequests atomic.Int32
	var activePageRequests atomic.Int32
	var maxActivePageRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequests.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}

		active := activePageRequests.Add(1)
		defer activePageRequests.Add(-1)
		for {
			currentMax := maxActivePageRequests.Load()
			if active <= currentMax || maxActivePageRequests.CompareAndSwap(currentMax, active) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Shared host audit page title</title></head><body><h1>Page</h1></body></html>"))
	}))
	defer server.Close()

	manager := newHostPolicyManager(time.Millisecond, 1, 16, time.Second)
	transport := &politeRoundTripper{base: server.Client().Transport, policies: manager}
	pageClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	robotsClient := newRobotsHTTPClient(transport)
	robotsCache := newRobotsPolicyCache(time.Hour, 16)

	firstURL := server.URL + "/page/1"
	secondURL := server.URL + "/page/2"
	jobs := make(chan AuditTarget, 2)
	jobs <- newAuditTarget(targetURLRecord{ID: 1, URL: firstURL}, firstURL, []byte(testTargetFingerprintKey))
	jobs <- newAuditTarget(targetURLRecord{ID: 2, URL: secondURL}, secondURL, []byte(testTargetFingerprintKey))
	close(jobs)
	results := make(chan Result, 2)

	var wg sync.WaitGroup
	for workerID := 1; workerID <= 2; workerID++ {
		wg.Add(1)
		go worker(
			context.Background(),
			context.Background(),
			workerID,
			jobs,
			results,
			pageClient,
			robotsClient,
			robotsCache,
			nil,
			Config{
				HTTPAttemptTimeout:   time.Second,
				HTTPTotalTimeout:     5 * time.Second,
				RobotsAttemptTimeout: time.Second,
				RobotsTotalTimeout:   5 * time.Second,
				MaxHTMLBodyBytes:     DefaultMaxHTMLBodyBytes,
			},
			&wg,
		)
	}
	wg.Wait()
	close(results)

	resultCount := 0
	for result := range results {
		resultCount++
		if result.Error != nil {
			t.Fatalf("worker returned error: %v", result.Error)
		}
	}
	if resultCount != 2 {
		t.Fatalf("unexpected result count: %d", resultCount)
	}
	if robotsRequests.Load() != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1", robotsRequests.Load())
	}
	if maxActivePageRequests.Load() != 1 {
		t.Fatalf("per-host concurrency reached %d, want 1", maxActivePageRequests.Load())
	}
}
