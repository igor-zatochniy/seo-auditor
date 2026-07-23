package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

	manager := newHostPolicyManager(50*time.Millisecond, 1, 16, time.Second)
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
	if elapsed := time.Since(started); elapsed < 35*time.Millisecond {
		t.Fatalf("per-host rate limit was not applied: elapsed=%s", elapsed)
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
