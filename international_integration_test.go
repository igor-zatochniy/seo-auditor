//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
	"github.com/pressly/goose/v3"
)

func TestInternationalAuditPersistsAndDiagnoses(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var origin string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/pl" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		canonical := origin + r.URL.Path
		if r.URL.Path == "/es" {
			canonical = origin + "/en"
		}
		fmt.Fprintf(w, `<head><title>Test</title><link rel="canonical" href="%s">`, canonical)
		fmt.Fprintf(w, `<link rel="alternate" hreflang="%s" href="%s%s">`, strings.TrimPrefix(r.URL.Path, "/"), origin, r.URL.Path)
		if r.URL.Path == "/en" {
			fmt.Fprintf(w, `<link rel="alternate" hreflang="es" href="%s/es"><link rel="alternate" hreflang="pl" href="%s/pl"><link rel="alternate" hreflang="en-UK" href="%s/?token=private-value">`, origin, origin, origin)
		}
		io.WriteString(w, `</head><body><h1>Page</h1>Content</body>`)
	}))
	defer fixture.Close()
	origin = fixture.URL
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{origin + "/en", origin + "/es", origin + "/pl"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/international", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-value") {
		t.Fatal("token leaked")
	}
	var data struct {
		Pages []international.Page `json:"pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Pages) != 3 {
		t.Fatal(len(data.Pages))
	}
	codes := map[string]bool{}
	for _, i := range data.Pages[0].Issues {
		codes[i.Code] = true
	}
	for _, code := range []string{"missing_return_link", "alternate_canonical_conflict", "alternate_http", "invalid_language", "missing_x_default"} {
		if !codes[code] {
			t.Fatal("missing", code, data.Pages[0].Issues)
		}
	}
	if data.Pages[2].HTML != nil {
		t.Fatal("404 received fabricated HTML evidence")
	}
	if w := strategyRequest(app, "GET", "/api/audits/"+newWebRunID()+"/international", ""); w.Code != 404 {
		t.Fatal("cross-run request did not fail", w.Code)
	}
}

func TestInternationalUpgradeFrom22PreservesReports(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := goose.DownToContext(t.Context(), db, migrationDir, 22); err != nil {
		t.Fatal(err)
	}
	if err := applySchemaMigrations(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("SEO history changed", count, err)
	}
	if err := app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("GEO history changed", count, err)
	}
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/international", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Pages []international.Page `json:"pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Pages {
		if p.Status != "not_checked" {
			t.Fatal("legacy evidence fabricated", p)
		}
	}
}

func TestSitemapHreflangPersistenceIsIdempotent(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/en"}); err != nil {
		t.Fatal(err)
	}
	site := &siteCrawl{Origin: "https://example.com"}
	e := map[string]*international.Evidence{"https://example.com/en": {Complete: true, Links: []international.Alternate{{Language: "en", URL: "https://example.com/en", Absolute: true, ValidURL: true}, {Language: "uk", URL: "https://example.com/uk?signature=sensitive", Absolute: true, ValidURL: true}}}}
	for range 2 {
		if err := saveSitemapHreflang(ctx, pool, cfg, site, e); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := pool.QueryRow(ctx, "SELECT evidence::text FROM audit_sitemap_hreflang WHERE run_id=$1", cfg.RunID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got international.Evidence
	if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got.Links) != 2 || !got.Complete || strings.Contains(raw, "sensitive") {
		t.Fatal(raw, err)
	}
}
