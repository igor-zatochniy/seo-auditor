package main

import (
	"context"
	"encoding/hex"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"net/url"
	"strings"
	"testing"
)

func TestCustomAuditRulesDistinguishFailuresAndUnknown(t *testing.T) {
	code := 200
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-rule-key")}
	raw := "https://example.com/pl/product"
	fp := fingerprintURL(cfg.TargetFingerprintKey, raw)
	page := auditRulePage{HTTP: &code, URL: raw, Fingerprint: fp, ScanStatus: "completed", Indexing: "allowed", Robots: "allowed", Complete: true, SelfCanonical: true,
		Signals:    &crawler.URLSignals{Complete: true, HTML: &crawler.URLReference{URL: raw, Valid: true, Fingerprints: []string{hex.EncodeToString(fp)}}},
		Structured: &structureddata.Report{Products: &structureddata.ProductEvidence{Count: 1, WithPrice: 1, Complete: true}}}
	rule := customAuditRule{ID: newWebRunID(), Name: "Правило", Pattern: "/pl/*", Kind: "self_canonical", Severity: "critical", Enabled: true}
	if e := validateAuditRule(rule); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"http_200", "indexable", "self_canonical", "canonical_prefix", "product_price"} {
		r := rule
		r.Kind = kind
		if kind == "canonical_prefix" {
			r.Value = "/pl/"
		}
		state, _, matched := evaluateAuditRule(r, page)
		if state != "pass" || !matched {
			t.Fatalf("%s: %s %v", kind, state, matched)
		}
	}
	page.Signals.HTML.URL = "https://example.com/en/product"
	page.SelfCanonical = false
	page.Signals.HTML.Fingerprints = []string{hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, page.Signals.HTML.URL))}
	state, _, _ := evaluateAuditRule(rule, page)
	if state != "fail" {
		t.Fatal("nonself canonical accepted")
	}
	rule.Kind = "indexable"
	page.Indexing = "blocked"
	state, _, _ = evaluateAuditRule(rule, page)
	if state != "fail" {
		t.Fatal("noindex accepted")
	}
	page.Complete = false
	state, _, _ = evaluateAuditRule(rule, page)
	if state != "unknown" {
		t.Fatal("partial HTML claimed verified")
	}
	rule.Pattern = "/else/*"
	_, _, matched := evaluateAuditRule(rule, page)
	if matched {
		t.Fatal("path scope lost")
	}
	rule.Pattern = "/*"
	rule.Kind = "eval"
	if validateAuditRule(rule) == nil {
		t.Fatal("arbitrary execution accepted")
	}
	rule.Kind = "http_200"
	rule.Pattern = "/**"
	if validateAuditRule(rule) == nil {
		t.Fatal("unbounded wildcard accepted")
	}
}

func TestRobotsDisallowMakesHTMLRulesUnknown(t *testing.T) {
	code := 200
	page := auditRulePage{URL: "https://example.com/pl/product", HTTP: &code, ScanStatus: "completed", Indexing: "allowed", Robots: "disallowed", Complete: true}
	for _, kind := range []string{"indexable", "self_canonical", "canonical_prefix", "product_price"} {
		rule := customAuditRule{Pattern: "/pl/*", Kind: kind}
		if kind == "canonical_prefix" {
			rule.Value = "/pl/"
		}
		state, evidence, matched := evaluateAuditRule(rule, page)
		if !matched || state != "unknown" || !strings.Contains(evidence, "robots.txt") {
			t.Fatalf("%s: state=%q matched=%v evidence=%q", kind, state, matched, evidence)
		}
	}
	rule := customAuditRule{Pattern: "/pl/*", Kind: "http_200"}
	state, _, matched := evaluateAuditRule(rule, page)
	if !matched || state != "pass" {
		t.Fatalf("HTTP status should remain independently verifiable: %q %v", state, matched)
	}
}

func TestCoverageCSVPreservesIdentityAndRejectsForeignURLs(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-csv-key")}
	records, e := parseCoverageCSV(context.Background(), cfg, "https://example.com", "Page,Clicks\nhttps://example.com/p?token=fixture-AAAA,1\nhttps://example.com/p?token=fixture-BBBB,2", true)
	if e != nil || len(records) != 2 {
		t.Fatal(e, len(records))
	}
	if string(records[0].Fingerprint) == string(records[1].Fingerprint) || records[0].URL != records[1].URL {
		t.Fatal("redaction used as identity")
	}
	for _, r := range records {
		if strings.Contains(r.URL, "fixture-") || !r.Important {
			t.Fatal("unsafe CSV record")
		}
	}
	if _, e = parseCoverageCSV(context.Background(), cfg, "https://example.com", "URL\nhttps://other.example/p", false); e == nil {
		t.Fatal("foreign origin accepted")
	}
	if _, e = parseCoverageCSV(context.Background(), cfg, "https://example.com", "URL\nhttps://example.com/p", false); e != nil {
		t.Fatal(e)
	}
	if len(facetWarnings(2, 1, 1, 4, 9999)) != 0 {
		t.Fatal("tiny sample called crawl trap")
	}
	if len(facetWarnings(100, 60, 70, 4, 1500)) != 4 {
		t.Fatal("large suspicious family not detected")
	}
}

func TestRuleCanonicalFactsAreComputedBeforeRedaction(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-canonical-key")}
	code := 200
	for _, v := range []struct{ target, canonical, want string }{
		{"https://example.com", "https://EXAMPLE.com:443/", "pass"},
		{"https://example.com/p?token=fixture-AAAA", "https://example.com/p?token=fixture-AAAA", "pass"},
		{"https://example.com/p?token=fixture-AAAA", "https://example.com/p?token=fixture-BBBB", "fail"},
	} {
		data := SEOData{URL: v.target, StatusCode: &code, HTMLSizeComplete: true, URLSignals: &crawler.URLSignals{Version: crawler.URLSignalsVersion, Complete: true, HTTP: []crawler.URLReference{{URL: v.canonical}}}}
		signals := secureURLSignals(data, cfg)
		page := auditRulePage{URL: redactURL(v.target), Fingerprint: fingerprintURL(cfg.TargetFingerprintKey, v.target), HTTP: &code, ScanStatus: "completed", Complete: true, Signals: signals}
		rule := customAuditRule{Pattern: "/*", Kind: "self_canonical"}
		state, _, matched := evaluateAuditRule(rule, page)
		if state != v.want || !matched {
			t.Fatalf("%s %s: %s", v.target, v.canonical, state)
		}
		if strings.Contains(signals.HTTP[0].URL, "fixture-") {
			t.Fatal("canonical query leaked")
		}
	}
}

func TestRuleScopeAcceptsSafeExactURLsAndBoundedPaths(t *testing.T) {
	for _, scope := range []string{"/products/*", "https://example.com/products/widget"} {
		rule := customAuditRule{ID: newWebRunID(), Name: "Scope", Pattern: scope, Kind: "http_200", Severity: "warning"}
		if err := validateAuditRule(rule); err != nil {
			t.Fatalf("valid scope %q rejected: %v", scope, err)
		}
	}
	for _, scope := range []string{"https://user:pass@example.com/p", "https://example.com/p?token=secret", "javascript:alert(1)", "/products/**"} {
		rule := customAuditRule{ID: newWebRunID(), Name: "Scope", Pattern: scope, Kind: "http_200", Severity: "warning"}
		if validateAuditRule(rule) == nil {
			t.Fatalf("unsafe scope accepted: %q", scope)
		}
	}
	target, _ := url.Parse("https://example.com/products/widget?token=private")
	if !auditRulePatternMatches("https://example.com/products/widget", target) {
		t.Fatal("exact URL scope should match its query variants without persisting query values")
	}
	root, _ := url.Parse("https://example.com/?token=private")
	if !auditRulePatternMatches("https://example.com", root) {
		t.Fatal("exact origin URL should match its root path")
	}
	if auditRulePatternMatches("https://other.example/products/widget", target) {
		t.Fatal("exact URL scope matched another origin")
	}
}

func TestCustomRuleComparisonsAndUnknownStates(t *testing.T) {
	code := 200
	page := auditRulePage{URL: "https://example.com/products/widget", HTTP: &code, ScanStatus: "completed", Complete: true, WordCount: 59, WordCountKnown: true}
	drop := customAuditRule{ID: newWebRunID(), Name: "Контент", Pattern: "/products/*", Kind: "content_drop", Operator: "decrease_percent", Threshold: 40, ConsecutiveRuns: 2, Severity: "warning"}
	if err := validateAuditRule(drop); err != nil {
		t.Fatal(err)
	}
	state, evidence, observed, matched := evaluateAuditRuleWithBaseline(drop, page, previousAuditRuleResult{observed: "100"})
	if !matched || state != "fail" || observed != "59" || !strings.Contains(evidence, "41%") {
		t.Fatalf("content drop: %q %q %q %v", state, evidence, observed, matched)
	}
	page.WordCount = 61
	state, _, _, _ = evaluateAuditRuleWithBaseline(drop, page, previousAuditRuleResult{observed: "100"})
	if state != "pass" {
		t.Fatalf("below threshold should pass: %q", state)
	}
	page.Complete = false
	state, _, _, _ = evaluateAuditRuleWithBaseline(drop, page, previousAuditRuleResult{observed: "100"})
	if state != "unknown" {
		t.Fatalf("partial crawl must be unknown: %q", state)
	}
	page.Complete = true
	page.WordCountKnown = false
	state, _, _, _ = evaluateAuditRuleWithBaseline(drop, page, previousAuditRuleResult{observed: "100"})
	if state != "unknown" {
		t.Fatalf("missing historical word count must be unknown: %q", state)
	}

	page.WordCountKnown = true
	page.Signals = &crawler.URLSignals{Complete: true, HTML: &crawler.URLReference{URL: "https://example.com/products/widget-v2", Valid: true}}
	canonical := customAuditRule{ID: newWebRunID(), Name: "Canonical", Pattern: "/products/*", Kind: "canonical_changed", Severity: "critical"}
	current, ok := canonicalRuleObservation(page.Signals)
	if !ok {
		t.Fatal("complete canonical should produce an observation")
	}
	state, evidence, next, matched := evaluateAuditRuleWithBaseline(canonical, page, previousAuditRuleResult{observed: current})
	if !matched || state != "pass" || next != current {
		t.Fatalf("unchanged canonical: %q %q", state, evidence)
	}
	page.Signals.HTML.URL = "https://example.com/products/widget"
	state, evidence, _, _ = evaluateAuditRuleWithBaseline(canonical, page, previousAuditRuleResult{observed: current})
	if state != "fail" || !strings.Contains(evidence, "відрізняється") {
		t.Fatalf("canonical change not detected: %q %q", state, evidence)
	}

	page.OAISearchRules = "blocked"
	oai := customAuditRule{ID: newWebRunID(), Name: "OAI", Pattern: "/products/*", Kind: "oai_search_allowed", Severity: "warning"}
	state, _, _, _ = evaluateAuditRuleWithBaseline(oai, page, previousAuditRuleResult{})
	if state != "fail" {
		t.Fatalf("blocked OAI-SearchBot should fail: %q", state)
	}
	page.OAISearchRules = "unknown"
	state, _, _, _ = evaluateAuditRuleWithBaseline(oai, page, previousAuditRuleResult{})
	if state != "unknown" {
		t.Fatalf("unverified OAI policy must remain unknown: %q", state)
	}
	drop.Operator = "execute_sql"
	if validateAuditRule(drop) == nil {
		t.Fatal("unsupported operator was accepted")
	}
}

func TestCustomRuleConsecutiveNotificationsAndExceptions(t *testing.T) {
	threshold := 2
	streak, notified, event := auditRuleTransition(previousAuditRuleResult{}, "fail", threshold, false)
	if streak != 1 || notified || event != "" {
		t.Fatalf("first failure fired too early: %d %v %q", streak, notified, event)
	}
	streak, notified, event = auditRuleTransition(previousAuditRuleResult{state: "fail", consecutive: 1}, "fail", threshold, false)
	if streak != 2 || !notified || event != "failed" {
		t.Fatalf("threshold failure was not notified: %d %v %q", streak, notified, event)
	}
	streak, notified, event = auditRuleTransition(previousAuditRuleResult{state: "fail", consecutive: 1}, "fail", threshold, true)
	if streak != 2 || notified || event != "" {
		t.Fatalf("exception did not suppress alert: %d %v %q", streak, notified, event)
	}
	_, notified, event = auditRuleTransition(previousAuditRuleResult{state: "fail", consecutive: 2}, "fail", threshold, false)
	if !notified || event != "failed" {
		t.Fatalf("expired exception should permit a pending alert: %v %q", notified, event)
	}
	_, _, event = auditRuleTransition(previousAuditRuleResult{state: "fail", consecutive: 2, notified: true}, "pass", threshold, false)
	if event != "resolved" {
		t.Fatalf("resolved failure should notify: %q", event)
	}
	_, _, event = auditRuleTransition(previousAuditRuleResult{state: "fail", consecutive: 2}, "unknown", threshold, false)
	if event != "" {
		t.Fatalf("unknown must not trigger alerts: %q", event)
	}

	previous := previousAuditRuleResult{}
	for run := 1; run <= 20; run++ {
		streak, notified, event = auditRuleTransition(previous, "fail", 10, false)
		if streak != min(run, 10) {
			t.Fatalf("streak exceeded storage bound on run %d: %d", run, streak)
		}
		if run == 10 && (event != "failed" || !notified) {
			t.Fatalf("threshold alert missing on run %d: %q %v", run, event, notified)
		}
		if run > 10 && event != "" {
			t.Fatalf("repeated failure generated duplicate alert on run %d: %q", run, event)
		}
		previous = previousAuditRuleResult{state: "fail", consecutive: streak, notified: notified}
	}
}

func TestCanonicalRuleObservationUsesHMACForRedactedQueryValues(t *testing.T) {
	redacted := "https://example.com/page?token=[REDACTED]"
	first := crawler.URLReference{URL: redacted, Valid: true, Fingerprints: []string{"hmac-first"}}
	second := crawler.URLReference{URL: redacted, Valid: true, Fingerprints: []string{"hmac-second"}}
	firstObservation, ok := canonicalRuleObservation(&crawler.URLSignals{Complete: true, HTML: &first})
	if !ok {
		t.Fatal("complete canonical observation was rejected")
	}
	secondObservation, ok := canonicalRuleObservation(&crawler.URLSignals{Complete: true, HTML: &second})
	if !ok {
		t.Fatal("complete canonical observation was rejected")
	}
	if firstObservation == secondObservation {
		t.Fatal("distinct HMAC identities for redacted canonical URLs were treated as unchanged")
	}
	first.Fingerprints = []string{"hmac-b", "hmac-a"}
	second.Fingerprints = []string{"hmac-a", "hmac-b"}
	firstObservation, _ = canonicalRuleObservation(&crawler.URLSignals{Complete: true, HTML: &first})
	secondObservation, _ = canonicalRuleObservation(&crawler.URLSignals{Complete: true, HTML: &second})
	if firstObservation != secondObservation {
		t.Fatal("fingerprint ordering changed a canonical observation")
	}
}
