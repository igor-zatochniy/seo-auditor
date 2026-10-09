package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/seo"
)

func TestRenderingRejectsPrivateResourcesAndHonorsRobots(t *testing.T) {
	cfg := Config{Workers: 1, RobotsTotalTimeout: time.Second, HTTPAttemptTimeout: time.Second}
	transport := newHTTPTransport(cfg, time.Second)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cache := newRobotsPolicyCache(time.Minute, 8)
	for _, address := range []string{"http://127.0.0.1/private", "http://169.254.169.254/latest/meta-data", "file:///etc/passwd", "http://user:pass@example.com/"} {
		if _, err := fetchRenderResource(context.Background(), address, cfg, client, client, cache); err == nil {
			t.Fatalf("unsafe URL accepted: %s", address)
		}
	}
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		hits++
		io.WriteString(w, "unsafe")
	}))
	defer server.Close()
	cfg.AllowPrivateTargets = true
	if _, err := fetchRenderResource(context.Background(), server.URL+"/private", cfg, server.Client(), server.Client(), newRobotsPolicyCache(time.Minute, 8)); err == nil || hits != 0 {
		t.Fatalf("robots bypass: %v, hits %d", err, hits)
	}
}

func TestRenderingStorageIsRedactedAndBounded(t *testing.T) {
	r := &seo.RenderComparison{Status: "completed", Raw: &seo.RenderSnapshot{Title: "secret https://example.com/a?token=hidden", Canonical: "https://example.com/a?X-Amz-Signature=private", TextSample: "a\x00b", Hreflang: []string{"en https://example.com/?secret=unsafe"}}}
	d := sanitizeSEODataForStorage(SEOData{Rendering: r})
	serialized := string(d.Rendering.JSON())
	for _, secret := range []string{"hidden", "private", "unsafe", "\\u0000"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("unredacted rendering field: %s", serialized)
		}
	}
	if len(serialized) > 200000 {
		t.Fatal("storage budget exceeded")
	}
}

func TestRenderingConfigurationAndCaptureBounds(t *testing.T) {
	cfg := Config{RenderJavaScript: true, RenderBrowserURL: "http://browser:9222", TargetLeaseDuration: 2 * time.Minute, HTTPTotalTimeout: 20 * time.Second, RobotsTotalTimeout: 10 * time.Second}
	if err := validateRendering(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.TargetLeaseDuration = 40 * time.Second
	if validateRendering(cfg) == nil {
		t.Fatal("accepted short lease")
	}
	for _, u := range []string{"", "file:///tmp/socket", "http://u:p@browser:9222", "http://browser:9222/path"} {
		cfg.RenderBrowserURL = u
		if validateRendering(cfg) == nil {
			t.Fatal("accepted endpoint")
		}
	}
	b := renderCapture{limit: 10}
	n, err := b.Write([]byte(strings.Repeat("x", 100)))
	if err != nil || n != 100 || b.Len() != 10 {
		t.Fatal("capture is not bounded")
	}
}
