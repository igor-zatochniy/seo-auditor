package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	robotsparser "github.com/igor-zatochniy/seo-auditor/internal/robots"
)

func TestGEOBotRulesReuseRobotsAndKeepCrawlerIdentity(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.UserAgent() != UserAgentStr {
			t.Errorf("Підмінено User-Agent: %s", r.UserAgent())
		}
		fmt.Fprint(w, "User-agent: *\nAllow: /\nUser-agent: Googlebot\nDisallow: /private\nUser-agent: OAI-SearchBot\nAllow: /\nUser-agent: GPTBot\nDisallow: /\n")
	}))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 4)
	for range 2 {
		allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL+"/private", time.Second)
		if err != nil || !allowed || search.GooglebotRules != geo.Blocked || search.OAISearchBotRules != geo.Allowed || search.GPTBotRules != geo.Blocked {
			t.Fatalf("Правила: %v %+v %v", allowed, search, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("Зайві запити robots.txt")
	}
}

func TestGEORulesRespectCacheBudgetWithoutBlockingCrawler(t *testing.T) {
	policy, err := robotsparser.CompilePolicy("User-agent: *\nAllow: /", UserAgentStr)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\nUser-agent: Googlebot\nDisallow: /\nUser-agent: OAI-SearchBot\nDisallow: /\n")
	}))
	defer server.Close()
	cache := newRobotsPolicyCacheWithLimits(time.Minute, 2, policy.EstimatedMemoryBytes(), 1)
	allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL, time.Second)
	if err != nil || !allowed || search.GooglebotRules != geo.Unknown || search.OAISearchBotRules != geo.Unknown || cache.weight > cache.maxWeight {
		t.Fatalf("Діагностика перевищила бюджет або завадила crawler: %+v %v", search, err)
	}
}

func TestGEOUnavailableRobotsIsUnknownNotAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 4)
	allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL, time.Second)
	if err == nil || allowed || search.GooglebotRules != geo.Unknown || search.OAISearchBotRules != geo.Unknown {
		t.Fatalf("Помилка перетворена на дозвіл: %+v", search)
	}
	for _, control := range search.AIPolicies {
		if control.Status != geo.Unknown || control.RobotsURL != server.URL+"/robots.txt" || control.CheckedAt == nil || control.HTTPStatus == nil || *control.HTTPStatus != http.StatusServiceUnavailable || control.Explanation == "" {
			t.Fatalf("Невідома політика втратила доказ джерела помилки: %+v", control)
		}
	}
	_, cachedSearch, cachedErr := cache.inspectTarget(context.Background(), server.Client(), server.URL, time.Second)
	if cachedErr == nil {
		t.Fatal("cached 503 unexpectedly became a successful robots policy")
	}
	for _, control := range cachedSearch.AIPolicies {
		if control.RobotsURL != server.URL+"/robots.txt" || control.HTTPStatus == nil || *control.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("cached unknown state lost HTTP evidence: %+v", control)
		}
	}
}

func TestAIDiscoverabilityMatrixIncludesProductTokensAndEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\nUser-agent: Googlebot\nDisallow: /private\nUser-agent: Google-Extended\nDisallow: /private\nUser-agent: OAI-SearchBot\nDisallow: /private\nUser-agent: GPTBot\nAllow: /\nUser-agent: Claude-SearchBot\nAllow: /\nUser-agent: ClaudeBot\nDisallow: /\nUser-agent: PerplexityBot\nAllow: /\nUser-agent: Applebot\nAllow: /\nUser-agent: Applebot-Extended\nDisallow: /\n")
	}))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 4)
	allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL+"/private?api_key=secret-value", time.Second)
	if err != nil || !allowed {
		t.Fatalf("SEO crawler verdict changed: allowed=%v err=%v", allowed, err)
	}
	if len(search.AIPolicies) != len(geo.SupportedAIPolicySpecs) {
		t.Fatalf("matrix controls=%d, want %d", len(search.AIPolicies), len(geo.SupportedAIPolicySpecs))
	}
	states := map[string]geo.AIControl{}
	for _, item := range search.AIPolicies {
		states[item.Token] = item
	}
	for token, want := range map[string]string{"Googlebot": geo.Blocked, "Google-Extended": geo.Blocked, "OAI-SearchBot": geo.Blocked, "GPTBot": geo.Allowed, "ClaudeBot": geo.Blocked, "Applebot-Extended": geo.Blocked} {
		if states[token].Status != want {
			t.Errorf("%s state=%s want=%s evidence=%+v", token, states[token].Status, want, states[token])
		}
	}
	for _, token := range []string{"Google-Extended", "Applebot-Extended"} {
		var spec *geo.AIPolicySpec
		for i := range geo.SupportedAIPolicySpecs {
			if geo.SupportedAIPolicySpecs[i].Token == token {
				spec = &geo.SupportedAIPolicySpecs[i]
			}
		}
		if spec == nil || spec.HasHTTPUserAgent {
			t.Errorf("%s incorrectly modeled as a standalone HTTP User-Agent", token)
		}
	}
	for _, item := range search.AIPolicies {
		if item.RobotsURL == "" || item.CheckedAt == nil || item.HTTPStatus == nil || *item.HTTPStatus != http.StatusOK {
			t.Errorf("missing source evidence for %s: %+v", item.Token, item)
		}
		if strings.Contains(item.Directive, "secret-value") {
			t.Errorf("sensitive query leaked through directive for %s: %q", item.Token, item.Directive)
		}
	}
}

func TestAIDiscoverabilityReportsAllowDisallowConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nAllow: /\nUser-agent: Googlebot\nDisallow: /private\nAllow: /private\n")
	}))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 2)
	_, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL+"/private", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range search.AIPolicies {
		if control.Token == "Googlebot" {
			if control.Status != geo.Allowed || !control.Conflict || control.Directive != "Allow: /private" || control.ConflictDirective != "Disallow: /private" {
				t.Fatalf("conflict was not explained correctly: %+v", control)
			}
			return
		}
	}
	t.Fatal("Googlebot control was not returned")
}

func TestAIDiscoverabilityUnavailableRobotsHasLimitedConfidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 2)
	allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL+"/page", time.Second)
	if err != nil || !allowed {
		t.Fatalf("RFC unavailable policy must keep crawler behavior: allowed=%v err=%v", allowed, err)
	}
	for _, control := range search.AIPolicies {
		if control.Status != geo.Allowed || control.Confidence != "robots_unavailable" || control.HTTPStatus == nil || *control.HTTPStatus != http.StatusNotFound {
			t.Fatalf("404 evidence was presented as a parsed robots policy: %+v", control)
		}
	}
}

func TestAIBotRuleLimitDoesNotBreakPrimaryCrawler(t *testing.T) {
	var body strings.Builder
	body.WriteString("User-agent: Googlebot\n")
	for i := range maxAIBotPolicyRules + 1 {
		fmt.Fprintf(&body, "Disallow: /private/%d\n", i)
	}
	body.WriteString("User-agent: *\nAllow: /\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body.String())) }))
	defer server.Close()
	cache := newRobotsPolicyCache(time.Minute, 2)
	allowed, search, err := cache.inspectTarget(context.Background(), server.Client(), server.URL+"/page", time.Second)
	if err != nil || !allowed {
		t.Fatalf("optional AI rule cap changed primary crawler behavior: allowed=%v err=%v", allowed, err)
	}
	if search.GooglebotRules != geo.Unknown || search.OAISearchBotRules != geo.Allowed {
		t.Fatalf("optional rule limit did not isolate the affected bot: %+v", search)
	}
	if cache.weight > cache.maxWeight {
		t.Fatalf("robots cache exceeded its weight bound: %d > %d", cache.weight, cache.maxWeight)
	}
}
