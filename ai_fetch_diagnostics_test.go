package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectFetchChallengeDoesNotTreatOrdinaryContentAsBlock(t *testing.T) {
	blocked, signals := detectFetchChallenge([]byte(`<html><title>Checking your browser</title><div>challenge-platform</div></html>`), 200)
	if !blocked || len(signals) < 2 {
		t.Fatalf("expected challenge markers on 200 response, got %v", signals)
	}
	blocked, signals = detectFetchChallenge([]byte(`<html><body>CAPTCHA required</body></html>`), 200)
	if !blocked || len(signals) == 0 {
		t.Fatalf("expected CAPTCHA challenge signal on 200 response, got %v", signals)
	}
	blocked, signals = detectFetchChallenge([]byte(`<html><body>Our CAPTCHA product helps secure forms.</body></html>`), 200)
	if blocked || len(signals) != 0 {
		t.Fatalf("ordinary content should not be reported as a challenge: %v", signals)
	}
	blocked, signals = detectFetchChallenge([]byte(`<html><body>This article explains why access denied errors happen.</body></html>`), 200)
	if blocked || len(signals) != 0 {
		t.Fatalf("ordinary 200 content must not be a challenge: %v", signals)
	}
	blocked, signals = detectFetchChallenge([]byte(`<html>captcha</html>`), 403)
	if !blocked || len(signals) != 1 {
		t.Fatalf("expected CAPTCHA marker with access denial, got %v", signals)
	}
}

func TestVisibleHTMLWordsExcludesInertAndExecutableText(t *testing.T) {
	body := []byte(`<main>Visible words here</main><script>secret script words</script><style>hidden style words</style><template>inactive template words</template>`)
	if words := visibleHTMLWords(body); words != 3 {
		t.Fatalf("visible word count = %d, want 3", words)
	}
}

func TestClassifyFetchResponseSeparatesChallengeAndHTTPFailures(t *testing.T) {
	cases := []struct {
		status int
		html   bool
		body   int
		block  bool
		want   string
	}{
		{200, true, 10, false, "html_received"},
		{200, true, 10, true, "challenge_suspected"},
		{429, true, 10, false, "http_rejected"},
		{503, true, 10, false, "http_rejected"},
		{200, false, 10, false, "non_html"},
		{200, true, 0, false, "empty_html"},
		{404, true, 10, false, "http_error"},
	}
	for _, test := range cases {
		if got := classifyFetchResponse(test.status, test.html, test.body, test.block); got != test.want {
			t.Errorf("classifyFetchResponse(%d,%t,%d,%t) = %q, want %q", test.status, test.html, test.body, test.block, got, test.want)
		}
	}
}

func TestFetchDiscrepancyRequiresSuccessfulAuditorHTML(t *testing.T) {
	auditor := aiFetchObservation{Status: "html_received"}
	bot := aiFetchObservation{Status: "http_rejected"}
	if !fetchDiscrepancy(auditor, bot) {
		t.Fatal("expected potential discrepancy")
	}
	auditor.Status = "network_error"
	if fetchDiscrepancy(auditor, bot) {
		t.Fatal("two unsuccessful probes are not an auditor-vs-bot discrepancy")
	}
}

func TestOnlyHTTPBotTokensCanBeSimulated(t *testing.T) {
	for _, unsupported := range []string{"google-extended", "applebot-extended", "unknown"} {
		if _, ok := simulatedAIBots[unsupported]; ok {
			t.Errorf("non-HTTP or unknown token %q must not be simulated", unsupported)
		}
	}
	for _, supported := range []string{"googlebot", "oai-searchbot", "gptbot"} {
		if bot, ok := simulatedAIBots[supported]; !ok || bot.UserAgent == "" || bot.Token == "" {
			t.Errorf("missing simulation mapping for %q", supported)
		}
	}
	for _, token := range []string{"oai-searchbot", "gptbot", "applebot"} {
		if userAgent := simulatedAIBots[token].UserAgent; !strings.HasPrefix(userAgent, "Mozilla/5.0") || !strings.Contains(strings.ToLower(userAgent), token) {
			t.Errorf("%s simulation lacks the documented browser-compatible User-Agent shape: %q", token, userAgent)
		}
	}
}

func TestProbeAIFetchHonorsApplebotGooglebotFallback(t *testing.T) {
	var pageRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: Googlebot\nDisallow: /blocked\nUser-agent: *\nAllow: /\n"))
			return
		}
		pageRequests.Add(1)
		_, _ = w.Write([]byte("<html>page</html>"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bot := simulatedAIBots["applebot"]
	result := probeAIFetch(ctx, server.URL+"/blocked", bot.Token, bot.UserAgent, server.Client(), server.Client(), newRobotsPolicyCache(time.Minute, 4), Config{RobotsTotalTimeout: time.Second})
	if result.Status != "blocked_by_robots" || pageRequests.Load() != 0 {
		t.Fatalf("Applebot must not fetch a Googlebot-blocked URL: %+v; page requests=%d", result, pageRequests.Load())
	}
}

func TestProbeAIFetchSeparatesRobotsAndSimulatedHTTPResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		if r.Header.Get("User-Agent") == simulatedAIBots["oai-searchbot"].UserAgent {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><body>Access denied</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><main>Public page content</main></body></html>"))
	}))
	defer server.Close()

	cfg := Config{RobotsTotalTimeout: time.Second}
	cache := newRobotsPolicyCache(time.Minute, 4)
	client := server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	auditor := probeAIFetch(ctx, server.URL+"/page", "", UserAgentStr, client, client, cache, cfg)
	bot := simulatedAIBots["oai-searchbot"]
	simulated := probeAIFetch(ctx, server.URL+"/page", bot.Token, bot.UserAgent, client, client, cache, cfg)
	if auditor.Status != "html_received" || auditor.VisibleWords != 3 || auditor.RobotsStatus != "allowed" {
		t.Fatalf("unexpected Auditor result: %+v", auditor)
	}
	if simulated.Status != "challenge_suspected" || !simulated.ChallengeSuspected || simulated.HTTPStatus != http.StatusForbidden {
		t.Fatalf("unexpected bot simulation: %+v", simulated)
	}
	if !fetchDiscrepancy(auditor, simulated) {
		t.Fatal("expected a potential simulated discrepancy")
	}
}

func TestProbeAIFetchDoesNotFollowCrossOriginRedirect(t *testing.T) {
	var destinationRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationRequests.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		http.Redirect(w, r, destination.URL+"/private", http.StatusFound)
	}))
	defer origin.Close()

	cfg := Config{RobotsTotalTimeout: time.Second}
	baseClient := origin.Client()
	client := &http.Client{Transport: baseClient.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := probeAIFetch(ctx, origin.URL+"/start", "", UserAgentStr, client, client, newRobotsPolicyCache(time.Minute, 4), cfg)
	if (result.Status != "external_redirect" && result.Status != "redirect_invalid") || destinationRequests.Load() != 0 {
		t.Fatalf("cross-origin redirect was followed or misclassified: result=%+v destination_requests=%d", result, destinationRequests.Load())
	}
}
