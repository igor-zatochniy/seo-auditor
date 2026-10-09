package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
}
