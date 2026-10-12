//go:build integration

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/duplicates"
	"github.com/pressly/goose/v3"
)

func duplicateAuditFixture(t *testing.T) (*webServer, Config) {
	t.Helper()
	ctx, pool, cfg := webIntegration(t)
	var canonical string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html lang="en"><head><title>&lt;img src=x onerror=alert(1)&gt;</title><link rel="canonical" href="%s"></head><body><nav>Menu %s</nav><main><h1>Merchant protection</h1><p>%s</p></main></body></html>`, canonical, r.URL.RawQuery, strings.Repeat("Payment safety protects merchants with evidence and careful monitoring. ", 40))
	}))
	t.Cleanup(fixture.Close)
	canonical = fixture.URL + "/page?token=private-value-A"
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{canonical, fixture.URL + "/page?token=private-value-B"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	return app, cfg
}

func TestDuplicateAuditPersistenceAndIsolation(t *testing.T) {
	app, cfg := duplicateAuditFixture(t)
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-value") || strings.Contains(w.Body.String(), "sha256") || strings.Contains(w.Body.String(), "fingerprint") {
		t.Fatal("private data leaked", w.Body.String())
	}
	var response struct {
		Summary  duplicates.Summary `json:"summary"`
		Clusters []duplicateCluster `json:"clusters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Summary.ExactClusters != 1 || response.Summary.SimilarPages != 2 || len(response.Clusters) != 1 || response.Clusters[0].Canonical != "consistent" || len(response.Clusters[0].Members) != 2 {
		t.Fatal(response)
	}
	if response.Clusters[0].Members[0].URL != response.Clusters[0].Members[1].URL || response.Clusters[0].Members[0].TargetID == response.Clusters[0].Members[1].TargetID {
		t.Fatal("redacted URL collisions merged target identities", response)
	}
	var stored int
	if err := app.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_results WHERE run_id=$1 AND content_signature->>'complete'='true'`, cfg.RunID).Scan(&stored); err != nil || stored != 2 {
		t.Fatal(stored, err)
	}
	for _, path := range []string{"?after=-1", "?cluster=abc", "?member_after=-1"} {
		if w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates"+path, ""); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	if w := strategyRequest(app, "GET", "/api/audits/"+newWebRunID()+"/duplicates", ""); w.Code != 404 {
		t.Fatal("cross-run isolation", w.Code)
	}
	if w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates?cluster=99999", ""); w.Code != 404 {
		t.Fatal("unknown group", w.Code)
	}
	if _, err := app.pool.Exec(t.Context(), `UPDATE audit_results SET content_signature=NULL WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	w = strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates", "")
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Summary.Unverified != 2 || response.Summary.SignaturesComplete || len(response.Clusters) != 0 {
		t.Fatal(w.Code, response)
	}
	if _, err := app.pool.Exec(t.Context(), `UPDATE audit_runs SET status='running',finished_at=NULL WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates", ""); w.Code != 409 {
		t.Fatal("running report not refused", w.Code)
	}
}

func TestDuplicateUpgradeFrom24PreservesSEOAndGEO(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 24); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("SEO history changed", count, err)
	}
	if err = app.pool.QueryRow(t.Context(), "SELECT count(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("GEO history changed", count, err)
	}
	if err = app.pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_results WHERE run_id=$1 AND content_signature IS NULL", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("legacy signatures fabricated", count, err)
	}
}

func TestDuplicateLegacyNegativeTargetsAreCountedAsUnverified(t *testing.T) {
	app, cfg := duplicateAuditFixture(t)
	if _, err := app.pool.Exec(t.Context(), `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,attempts,finished_at) VALUES($1,-1,'','completed',1,CURRENT_TIMESTAMP)`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(t.Context(), `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status)
	 SELECT run_id,-1,'https://example.com/legacy',target_fingerprint,fingerprint_key_id,200,'completed' FROM audit_results WHERE run_id=$1 AND target_id=1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates", "")
	var response struct {
		Summary duplicates.Summary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Summary.Pages != 3 || response.Summary.Unverified != 1 || response.Summary.SignaturesComplete {
		t.Fatal(w.Code, response)
	}
}
