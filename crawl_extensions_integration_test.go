//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/performance"
)

func extensionAPI(t *testing.T, s *webServer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+s.web.AccessToken)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-SEO-Auditor-Request", "1")
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func extensionApp(cfg Config) *webServer {
	s := &webServer{cfg: cfg, queries: make(chan struct{}, 3), submissions: make(chan struct{}, 1)}
	s.web.AccessToken = "local-development-only-web-test-token"
	return s
}

func TestURLRoutesUseSavedIdentityAndDetectRealConflicts(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/robots.txt":
			http.NotFound(w, r)
			return
		case "/r":
			http.Redirect(w, r, "/r2", 301)
			return
		case "/r2":
			http.Redirect(w, r, "/c", 302)
			return
		case "/x":
			http.Redirect(w, r, "/y", 301)
			return
		case "/y":
			http.Redirect(w, r, "/x", 301)
			return
		case "/c":
			http.NotFound(w, r)
			return
		case "/a":
			w.Header().Set("Link", `<`+base+`/b?token=fixture-route-secret>; rel="canonical"`)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<head><title>Root</title></head><body><a href="/a">A</a><a href="/b?token=fixture-route-secret">B</a><a href="/c">C</a><a href="/r">R</a><a href="/x">X</a></body>`)
		case "/a":
			fmt.Fprintf(w, `<head><title>A</title><link rel="canonical" href="%s/c"></head><body>A</body>`, base)
		case "/b":
			fmt.Fprintf(w, `<head><title>B</title><meta name="robots" content="noindex"><link rel="canonical" href="%s/b?token=fixture-route-secret"></head><body>B</body>`, base)
		}
	}))
	defer fixture.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 20, MaxDepth: 5}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	s := extensionApp(cfg)
	s.pool = pool
	w := extensionAPI(t, s, "GET", "/api/audits/"+cfg.RunID+"/routes", nil)
	if w.Code != 200 {
		t.Fatalf("routes: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "fixture-route-secret") {
		t.Fatal("route secret leaked")
	}
	var report struct{ Items []routePage }
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := map[string]routePage{}
	for _, item := range report.Items {
		found[strings.Split(strings.TrimPrefix(item.URL, fixture.URL), "?")[0]] = item
	}
	if found["/r"].Redirect.State != "http_error" || len(found["/r"].Redirect.Steps) != 3 {
		t.Fatalf("redirect=%+v", found["/r"])
	}
	if found["/x"].Redirect.State != "loop" {
		t.Fatal("redirect cycle not detected")
	}
	if found["/a"].Canonical.State != "http_error" || len(found["/a"].Conflicts) != 1 {
		t.Fatalf("canonical=%+v", found["/a"])
	}
	if found["/b"].Canonical.State != "noindex" {
		t.Fatalf("indexability=%+v", found["/b"])
	}
	if found["/a"].Inlinks != 1 {
		t.Fatal("affected source pages missing")
	}
	w = extensionAPI(t, s, "GET", fmt.Sprintf("/api/audits/%s/routes/%d/inlinks", cfg.RunID, found["/a"].TargetID), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), fixture.URL+"/") {
		t.Fatalf("inlinks=%s", w.Body.String())
	}
}

func TestCrUXConsentCachingAndRunIsolation(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	original := "https://example.com/file?token=fixture-crux-secret"
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{original}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status) VALUES($1,1,$2,$3,$4,200,'completed')`, cfg.RunID, redactURL(original), fingerprintURL(cfg.TargetFingerprintKey, original), cfg.TargetFingerprintKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status='completed',started_at=CURRENT_TIMESTAMP,finished_at=CURRENT_TIMESTAMP WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=CURRENT_TIMESTAMP,successful_urls=1 WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	cfg.CrUXAPIKey = "local-development-only-crux-key"
	s := extensionApp(cfg)
	s.pool = pool
	var calls atomic.Int32
	s.cruxQuery = func(_ context.Context, key, address, device, scope string) (*performance.CrUXReport, error) {
		calls.Add(1)
		if address != "https://example.com/file" || key != cfg.CrUXAPIKey {
			t.Fatal("secret or wrong scope sent to provider")
		}
		return &performance.CrUXReport{Status: "no_data", Scope: scope, Device: device, Requested: address, Metrics: map[string]performance.FieldMetric{}}, nil
	}
	path := fmt.Sprintf("/api/audits/%s/results/1/crux", cfg.RunID)
	w := extensionAPI(t, s, "POST", path, map[string]any{"confirm_public_url": false})
	if w.Code != 400 || calls.Load() != 0 {
		t.Fatal("provider called without consent")
	}
	for i := 0; i < 2; i++ {
		w = extensionAPI(t, s, "POST", path, map[string]any{"confirm_public_url": true})
		if w.Code != 200 {
			t.Fatalf("crux: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("fresh cache did not avoid network request")
	}
	if strings.Contains(w.Body.String(), "fixture-crux-secret") || strings.Contains(w.Body.String(), cfg.CrUXAPIKey) {
		t.Fatal("CrUX key/query leaked")
	}
	w = extensionAPI(t, s, "GET", path, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "no_data") {
		t.Fatal("history missing")
	}
	w = extensionAPI(t, s, "GET", "/api/audits/"+newWebRunID()+"/results/1/crux", nil)
	if w.Code != 404 {
		t.Fatal("cross-run history accessible")
	}
}

func TestSiteRenderedLinksEnterTheSameBoundedFrontier(t *testing.T) {
	endpoint := os.Getenv("RENDER_TEST_BROWSER_URL")
	if endpoint == "" {
		t.Skip("RENDER_TEST_BROWSER_URL не налаштовано")
	}
	ctx, pool, cfg := webIntegration(t, 2*time.Minute)
	cfg.RenderJavaScript = true
	cfg.RenderBrowserURL = endpoint
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<html><head><title>Raw root</title></head><body><a href="/shared">Shared</a><script>document.title='Rendered root';document.body.insertAdjacentHTML('beforeend','<a href="/js?token=fixture-dom-secret">Only JS</a><a href="/nofollow" rel="nofollow">No follow</a>');</script></body></html>`)
		} else {
			fmt.Fprint(w, `<html><head><title>Child</title></head><body>Child</body></html>`)
		}
	}))
	defer fixture.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 10, MaxDepth: 2, DiscoverRendered: true}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	var total, js, both int
	var title string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE safe_url LIKE '%/js?%') FROM audit_results WHERE run_id=$1`, cfg.RunID).Scan(&total, &js); err != nil || total != 3 || js != 1 {
		t.Fatalf("total=%d js=%d err=%v", total, js, err)
	}
	if err := pool.QueryRow(ctx, `SELECT title FROM audit_results WHERE run_id=$1 AND target_id=1`, cfg.RunID).Scan(&title); err != nil || title != "Raw root" {
		t.Fatal("raw SEO replaced")
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_site_edges WHERE run_id=$1 AND discovery_source='both'`, cfg.RunID).Scan(&both); err != nil || both != 1 {
		t.Fatalf("both=%d err=%v", both, err)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT json_agg(e)::TEXT FROM audit_site_edges e WHERE run_id=$1`, cfg.RunID).Scan(&raw); err != nil || strings.Contains(raw, "fixture-dom-secret") {
		t.Fatal("DOM URL leaked")
	}
}
