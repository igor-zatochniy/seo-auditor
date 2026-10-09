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
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/seo"
)

func TestRenderingPersistsBothStatesWithoutReplacingSEO(t *testing.T) {
	endpoint := os.Getenv("RENDER_TEST_BROWSER_URL")
	if endpoint == "" {
		t.Skip("RENDER_TEST_BROWSER_URL не налаштовано")
	}
	ctx, pool, cfg := webIntegration(t)
	cfg.RenderJavaScript = true
	cfg.RenderBrowserURL = endpoint
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Response title</title><meta name="description" content="Response description"><link rel="canonical" href="/before"><meta name="robots" content="index"></head><body><p>Initial text.</p><script>
document.title='Rendered title';document.querySelector('meta[name=description]').content='Rendered description';document.querySelector('link[rel=canonical]').href='/after?token=do-not-store';document.querySelector('meta[name=robots]').content='noindex';document.head.insertAdjacentHTML('beforeend','<link rel="alternate" hreflang="uk" href="/uk">');const data=document.createElement('script');data.type='application/ld+json';data.textContent='{"@type":"Article"}';document.head.append(data);document.body.insertAdjacentHTML('beforeend','<h1>Rendered H1</h1><p>More useful page content appears now.</p><a href="/new">New link</a>');</script></body></html>`)
	}))
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	var title, status string
	var comparison *seo.RenderComparison
	if err := pool.QueryRow(ctx, "SELECT title,scan_status,rendering FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&title, &status, &comparison); err != nil {
		t.Fatal(err)
	}
	if title != "Response title" || status != "completed" || comparison == nil || comparison.Status != "completed" || comparison.Rendered.Title != "Rendered title" || !comparison.Delta.JSONLDInjected || !comparison.Delta.H1OnlyAfterRendering || !comparison.Delta.Hreflang {
		t.Fatalf("incorrect states: title=%s comparison=%+v", title, comparison)
	}
	if strings.Contains(string(comparison.JSON()), "do-not-store") {
		t.Fatal("URL leaked into rendering JSON")
	}
	if comparison.Performance == nil || comparison.Performance.Status != "completed" || comparison.Performance.LCPMS == nil || comparison.Performance.CLS == nil {
		t.Fatalf("laboratory measurements lost: %+v", comparison.Performance)
	}
	run, err := loadWebRun(ctx, pool, cfg, cfg.RunID)
	if err != nil || !run.RenderJavaScript {
		t.Fatalf("run option lost: %v", err)
	}
	page, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: 0, Limit: 50, Filter: "javascript_changes"})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("API failed: %v", err)
	}
	var report bytes.Buffer
	var rendering map[string]any
	if err := json.Unmarshal(comparison.JSON(), &rendering); err != nil {
		t.Fatal(err)
	}
	page.Rows[0]["rendering"] = rendering
	if err := renderFullReport(&report, run, webAnalytics{}, func(visit func(reportRecord) error) error { return visit(page.Rows[0]) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "Rendered title") || !strings.Contains(report.String(), "Response title") {
		t.Fatal("export lost rendering")
	}
	var csv bytes.Buffer
	if err := writeCSVReport(&csv, func(visit func(reportRecord) error) error { return visit(page.Rows[0]) }); err != nil || !strings.Contains(csv.String(), "Rendered title") {
		t.Fatalf("CSV missing comparison: %v", err)
	}
	// Відновлення використовує збережений режим навіть після зміни ENV.
	if _, err := pool.Exec(ctx, "UPDATE audit_runs SET status='failed' WHERE id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	cfg.RenderJavaScript = false
	if err := createAuditRun(ctx, pool, &cfg); err != nil || !cfg.RenderJavaScript {
		t.Fatalf("resume lost mode: %v", err)
	}
}

func TestRenderingFailureKeepsResponseResult(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	cfg.RenderJavaScript = true
	cfg.RenderBrowserURL = "http://127.0.0.1:1"
	fixture := webFixture()
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("render failure broke SEO: %d", code)
	}
	var status, title, renderStatus string
	if err := pool.QueryRow(ctx, "SELECT scan_status,title,rendering->>'status' FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&status, &title, &renderStatus); err != nil || status != "completed" || title == "" || renderStatus != "failed" {
		t.Fatalf("lost raw result: %s %s %s %v", status, title, renderStatus, err)
	}
	var encoded []byte
	if err := pool.QueryRow(ctx, "SELECT rendering FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&encoded); err != nil || !json.Valid(encoded) {
		t.Fatal("invalid JSON")
	}
}

func TestRenderingDisabledOnExistingAudits(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := webFixture()
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	page, err := loadResultPage(context.Background(), pool, cfg.RunID, resultQuery{After: 0, Limit: 1, Filter: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Rows[0]["rendering"] != nil || page.Rows[0]["rendering_status"] != "disabled" {
		t.Fatal("normal audit unexpectedly rendered")
	}
}
