//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func webIntegration(t *testing.T) (context.Context, *pgxpool.Pool, Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required")
	}
	applyIntegrationMigrations(t, ctx, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Setenv("TARGET_FINGERPRINT_KEY", "local-development-only-fingerprint-key")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunID = newWebRunID()
	cfg.WorkerInstanceID = "web-integration"
	cfg.Workers = 2
	cfg.AllowPrivateTargets = true
	cfg.RateLimitInterval = time.Millisecond
	cfg.AuditRunHeartbeatInterval = 100 * time.Millisecond
	cfg.ShutdownTimeout = time.Second
	cfg.FinalizationTimeout = 5 * time.Second
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = pool.Exec(cleanup, "DELETE FROM audit_runs WHERE worker_instance_id='web-integration'")
	})
	return ctx, pool, cfg
}

func webFixture() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://example.com/?token=private-value")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head><title>A complete test page for the local SEO auditor</title><meta name="description" content="A test description"><meta name="viewport" content="width=device-width"><link rel="canonical" href="https://example.com/?token=private-value"></head><body><h1>Integration H1</h1><p>One two three</p><a href="/next">Next</a><img src="/image.jpg"></body></html>`)
	}))
}

func TestWebExplicitRunIsAtomicAndIndependent(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var sourceCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pages_to_scan").Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	urls := []string{"https://example.com/a?token=alpha", "https://example.com/a?token=beta"}
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	var total int
	var captured bool
	if err := pool.QueryRow(ctx, "SELECT total_urls, targets_captured_at IS NOT NULL FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&total, &captured); err != nil || total != 2 || !captured {
		t.Fatalf("total=%d captured=%t err=%v", total, captured, err)
	}
	var got []string
	if err := pool.QueryRow(ctx, "SELECT array_agg(request_url ORDER BY target_id) FROM audit_run_targets WHERE run_id=$1", cfg.RunID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join(urls, "\n") {
		t.Fatal("global targets leaked into explicit snapshot")
	}
	bad := cfg
	bad.RunID = newWebRunID()
	if err := createExplicitAuditRun(ctx, pool, &bad, []string{urls[0], "bad\x00target"}); err == nil {
		t.Fatal("invalid target insert succeeded")
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM audit_runs WHERE id=$1)", bad.RunID).Scan(&exists); err != nil || exists {
		t.Fatal("partial explicit run persisted")
	}
	var after int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM pages_to_scan").Scan(&after)
	if after != sourceCount {
		t.Fatal("global source mutated")
	}
}

func TestWebPipelineReportAnalyticsAndPagination(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := webFixture()
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page?token=secret-alpha", fixture.URL + "/redirect"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	run, err := loadWebRun(ctx, pool, cfg, cfg.RunID)
	if err != nil || run.Total != 2 || run.Status != "completed" {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	var retained int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1 AND request_url<>''", cfg.RunID).Scan(&retained)
	if retained != 0 {
		t.Fatal("raw URLs retained")
	}
	page, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 1, Filter: "all"})
	if err != nil || len(page.Rows) != 1 || page.Next != "1" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	second, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: 1, Limit: 1, Filter: "all"})
	if err != nil || len(second.Rows) != 1 || fmt.Sprint(second.Rows[0]["target_id"]) != "2" || second.Next != "" {
		t.Fatal("pagination lost or duplicated rows")
	}
	if len(page.Rows[0]) != len(reportFields) {
		t.Fatalf("fields=%d want=%d", len(page.Rows[0]), len(reportFields))
	}
	if page.Rows[0]["title_width_px"] == nil || page.Rows[0]["description_width_px"] == nil ||
		fmt.Sprint(page.Rows[0]["title_status"]) != "Recommended" ||
		fmt.Sprint(page.Rows[0]["description_mobile_status"]) != "Safe" {
		t.Fatalf("pixel metrics not persisted: %+v", page.Rows[0])
	}
	if second.Rows[0]["title_width_px"] != nil || second.Rows[0]["description_width_px"] != nil {
		t.Fatal("unparsed redirect has pixel measurements")
	}
	recommended, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 50, Filter: "title_recommended"})
	if err != nil || len(recommended.Rows) != 1 {
		t.Fatalf("pixel filter failed: %+v %v", recommended, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "secret-alpha") || strings.Contains(string(encoded), "request_url") || strings.Contains(string(encoded), "target_fingerprint") {
		t.Fatal("API privacy failure")
	}
	a, err := loadWebAnalytics(ctx, pool, cfg.RunID)
	if err != nil || a.Total != 2 || a.Parsed != 1 {
		t.Fatalf("analytics=%+v error=%v", a, err)
	}
	if a.Groups[0].Buckets[0].Value != 1 || a.Groups[0].Buckets[1].Value != 1 {
		t.Fatal("HTTP distribution mismatch")
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	for _, format := range []string{"html", "csv"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/audits/"+cfg.RunID+"/export/"+format, nil)
		r.Header.Set("Authorization", "Bearer "+testWebToken)
		w := httptest.NewRecorder()
		app.handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Integration H1") {
			t.Fatalf("export %s status=%d %s", format, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-alpha") || strings.Contains(w.Body.String(), "private-value") {
			t.Fatal("export leaked URL query value")
		}
		if !strings.Contains(w.Body.String(), "Recommended") || !strings.Contains(w.Body.String(), "liberation-sans-2.1.5") {
			t.Fatal("export omitted pixel statuses or measurement model")
		}
	}
}

func TestWebRunResumesCapturedTargetsAfterCrash(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := webFixture()
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/one", fixture.URL + "/two"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimTargetURLBatch(ctx, pool, cfg, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	target := newAuditTarget(claimed[0], claimed[0].URL, cfg.TargetFingerprintKey)
	if err := markAuditRunTargetStarted(ctx, pool, target, cfg); err != nil {
		t.Fatal(err)
	}
	results := make(chan Result, 1)
	results <- Result{Target: target, Data: SEOData{URL: target.RequestURL, StatusCode: httpStatus(200), ScanStatus: scanStatusCompleted, Title: "Already saved", RobotsOutcome: robotsOutcomeAllowed}}
	close(results)
	if summary := saveResults(ctx, pool, results, cfg); summary.Saved != 1 {
		t.Fatal(summary)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_runs SET heartbeat_at=CURRENT_TIMESTAMP-INTERVAL '1 hour' WHERE id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	m := newAuditManager(ctx, pool, cfg)
	m.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	if _, err := m.start(ctx, nil, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var total, generation int
	if err := pool.QueryRow(ctx, "SELECT total_urls,owner_generation FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&total, &generation); err != nil || total != 2 || generation != 2 {
		t.Fatalf("total=%d generation=%d err=%v", total, generation, err)
	}
	var title string
	_ = pool.QueryRow(ctx, "SELECT title FROM audit_results WHERE run_id=$1 AND target_id=1", cfg.RunID).Scan(&title)
	if title != "Already saved" {
		t.Fatal("completed target was scanned again")
	}
}

func TestWebCancelBeforeFirstSnapshotRead(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/a", "https://example.com/b"}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if code := executeCapturedAuditRun(canceled, pool, cfg, false); code != exitCanceled {
		t.Fatalf("exit=%d", code)
	}
	run, err := loadWebRun(ctx, pool, cfg, cfg.RunID)
	if err != nil || run.Total != 2 || run.Status != auditRunStatusCanceled {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	var unfinished int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1 AND (request_url<>'' OR status IN ('pending','running'))", cfg.RunID).Scan(&unfinished)
	if unfinished != 0 {
		t.Fatal("cancel left unfinished targets or raw URLs")
	}
}

func TestWebAPICancelAndExclusiveManager(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>Page</title><p>Text</p>")
	}))
	defer fixture.Close()
	m := newAuditManager(ctx, pool, cfg)
	m.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	app.manager = m
	h := app.handler()
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://127.0.0.1")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-SEO-Auditor-Request", "1")
		r.Header.Set("Authorization", "Bearer "+testWebToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body, _ := json.Marshal(map[string]string{"urls": fixture.URL + "/one\n" + fixture.URL + "/two"})
	w := post("/api/audits", string(body))
	if w.Code != 202 {
		close(release)
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	if w := post("/api/audits", string(body)); w.Code != 409 {
		close(release)
		t.Fatalf("parallel audit: %d", w.Code)
	}
	w = post("/api/audits/"+response.ID+"/cancel", "{}")
	close(release)
	if w.Code != 202 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	m.shutdown()
	run, err := loadWebRun(ctx, pool, cfg, response.ID)
	if err != nil || run.Status != auditRunStatusCanceled {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	var retained int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM audit_run_targets WHERE run_id=$1 AND request_url<>''", response.ID).Scan(&retained)
	if retained != 0 {
		t.Fatal("canceled run retained raw URLs")
	}
}
