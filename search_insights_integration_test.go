//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func searchFixture(t *testing.T) (context.Context, *webServer, Config) {
	t.Helper()
	ctx, pool, cfg := webIntegration(t)
	urls := []string{"https://EXAMPLE.com/", "https://example.com/products/a", "https://example.com/products/b?token=fixture-secret"}
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	for i, u := range urls {
		_, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,is_self_canonical,geo_signals)
 VALUES($1,$2,$3,$4,$5,200,'completed',TRUE,$6)`, cfg.RunID, i+1, redactURL(u), fingerprintURL(cfg.TargetFingerprintKey, u), cfg.TargetFingerprintKeyID, map[string]any{"search": map[string]any{"google_indexing": map[bool]string{true: "blocked", false: "allowed"}[i == 2]}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=NOW(),successful_urls=3 WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	s := extensionApp(cfg)
	s.pool = pool
	return ctx, s, cfg
}

func TestSearchLandingHistoryAndStructure(t *testing.T) {
	ctx, s, cfg := searchFixture(t)
	base := "/api/audits/" + cfg.RunID
	reportID := newWebRunID()
	_, err := s.pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count) VALUES($1,$2,$3,'example.com','local-lexical-v4',1,3,1,0)`, reportID, cfg.RunID, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	target := int64(2)
	_, err = s.pool.Exec(ctx, `INSERT INTO geo_query_results(report_id,ordinal,result) VALUES($1,1,$2)`, reportID, geo.Result{Query: "test query", TargetID: &target, TargetURL: "https://example.com/products/a"})
	if err != nil {
		t.Fatal(err)
	}
	csv := searchHeader + "2026-10-01,test query,https://example.com/products/a,US,desktop,100,10,2\n" +
		"2026-10-01,test query,https://example.com/products/b?token=fixture-secret,US,desktop,20,1,5\n" +
		"2026-10-02,test query,https://example.com/products/a,US,desktop,10,1,4\n" +
		"2026-10-02,test query,https://example.com/products/b?token=fixture-secret,US,desktop,110,3,3\n" +
		"2026-10-02,test query,https://example.com/products/a,PL,mobile,90,8,2\n"
	importID := newWebRunID()
	payload := map[string]any{"id": importID, "origin": "https://example.com", "csv": csv}
	for i := 0; i < 2; i++ {
		w := extensionAPI(t, s, "POST", base+"/search-imports", payload)
		if w.Code != 201 && w.Code != 200 {
			t.Fatalf("import %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := extensionAPI(t, s, "GET", base+"/landing-pages", nil)
	if w.Code != 200 {
		t.Fatalf("landing %d %s", w.Code, w.Body.String())
	}
	var data struct{ Items []map[string]any }
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 2 {
		t.Fatalf("segments=%v", data.Items)
	}
	for _, v := range data.Items {
		if v["country"] == "USA" {
			if v["changed"] != true || v["target_mismatch"] != true || v["expected_url"] != "https://example.com/products/a" {
				t.Fatalf("leader=%v", v)
			}
		} else if v["changed"] != nil {
			t.Fatal("invented previous day")
		}
	}
	if strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatal("secret leaked")
	}
	w = extensionAPI(t, s, "GET", base+"/landing-history?query=test+query&country=US&device=desktop", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatalf("history %d %s", w.Code, w.Body.String())
	}
	// A duplicate key from another file must roll back the entire new import.
	w = extensionAPI(t, s, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": searchHeader + "2026-10-03,new query,https://example.com/,US,desktop,1,0,2\n" + strings.Split(csv, "\n")[1] + "\n"})
	if w.Code != 409 {
		t.Fatalf("conflict=%d %s", w.Code, w.Body.String())
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_search_rows WHERE run_id=$1`, cfg.RunID).Scan(&count); err != nil || count != 5 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	w = extensionAPI(t, s, "GET", base+"/structure", nil)
	if w.Code != 200 {
		t.Fatalf("structure %d %s", w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range data.Items {
		if v["path"] == "/products/" {
			found = true
			if v["pages"] != float64(2) || v["noindex"] != float64(1) || v["geo_pages"] != float64(1) || v["bot_visits"] != nil || v["clicks"] != float64(12) || v["previous_clicks"] != float64(11) || v["traffic_comparable"] != false {
				t.Fatalf("section=%v", v)
			}
		}
	}
	if !found {
		t.Fatal("products not found")
	}
	w = extensionAPI(t, s, "GET", base+"/structure?prefix="+url.QueryEscape("/products/"), nil)
	if w.Code != 200 {
		t.Fatalf("nested %s", w.Body.String())
	}
	manualTarget := int64(3)
	_, err = s.pool.Exec(ctx, `INSERT INTO geo_target_decisions(id,report_id,ordinal,source_run_id,target_id,selected_result,reason,payload_hash) VALUES($1,$2,1,$3,3,$4,'manual fixture',$5)`, newWebRunID(), reportID, cfg.RunID, geo.Result{Query: "test query", TargetID: &manualTarget, TargetURL: redactURL("https://example.com/products/b?token=fixture-secret")}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	w = extensionAPI(t, s, "GET", base+"/landing-pages?country=US&device=desktop", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 1 || data.Items[0]["target_mismatch"] != false {
		t.Fatalf("manual mapping ignored: %v", data.Items)
	}
	rotatedCfg := cfg
	rotatedCfg.TargetFingerprintKey = []byte("local-development-only-rotated-secret")
	rotatedApp := extensionApp(rotatedCfg)
	rotatedApp.pool = s.pool
	w = extensionAPI(t, rotatedApp, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": searchHeader + "2026-10-03,new,https://example.com/,US,mobile,1,0,2\n"})
	if w.Code != 409 {
		t.Fatalf("same-id key rotation accepted: %d %s", w.Code, w.Body.String())
	}
	if _, err = s.pool.Exec(ctx, `UPDATE audit_search_imports SET key_check=NULL WHERE id=$1`, importID); err != nil {
		t.Fatal(err)
	}
	w = extensionAPI(t, s, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": searchHeader + "2026-10-03,new,https://example.com/,US,mobile,1,0,2\n"})
	if w.Code != 409 {
		t.Fatalf("unverified legacy key mixed: %d", w.Code)
	}
	w = extensionAPI(t, s, "GET", base+"/landing-pages", nil)
	if w.Code != 200 {
		t.Fatal("legacy data no longer readable")
	}
	w = extensionAPI(t, s, "DELETE", base+"/search-imports/"+importID, map[string]any{})
	if w.Code != 200 {
		t.Fatalf("delete %d", w.Code)
	}
	if err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_search_rows WHERE run_id=$1`, cfg.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("delete cascade failed")
	}
}

func TestSearchInsightsTiePrivacyIsolationAndPagination(t *testing.T) {
	ctx, s, cfg := searchFixture(t)
	base := "/api/audits/" + cfg.RunID
	var raw strings.Builder
	raw.WriteString(searchHeader)
	for i := 0; i < 55; i++ {
		fmt.Fprintf(&raw, "2026-10-01,q%03d,https://example.com/products/a,US,mobile,1,0,2\n", i)
	}
	raw.WriteString("2026-10-01,tie,https://example.com/file?token=fixture-a,US,mobile,10,0,2\n2026-10-01,tie,https://example.com/file?token=fixture-b,US,mobile,10,0,2\n")
	w := extensionAPI(t, s, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": raw.String()})
	if w.Code != 201 {
		t.Fatalf("import %d %s", w.Code, w.Body.String())
	}
	w = extensionAPI(t, s, "GET", base+"/landing-pages?offset=50", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var data struct {
		Items []map[string]any
		More  bool
	}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	if len(data.Items) != 6 || data.More {
		t.Fatalf("page=%v", data)
	}
	last := data.Items[5]
	if last["query"] != "tie" || last["current_url"] != nil || last["changed"] != nil || last["tied"] != true {
		t.Fatalf("tie=%v", last)
	}
	w = extensionAPI(t, s, "GET", base+"/landing-history?query=tie&country=US&device=mobile", nil)
	if strings.Contains(w.Body.String(), "fixture-a") || strings.Contains(w.Body.String(), "fixture-b") {
		t.Fatal("query secret leaked")
	}
	var history struct{ Items []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &history)
	if len(history.Items) != 2 || history.Items[0]["url_id"] == history.Items[1]["url_id"] {
		t.Fatal("redacted variants merged")
	}
	other := cfg
	other.RunID = newWebRunID()
	if err := createExplicitAuditRun(ctx, s.pool, &other, []string{"https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,scan_status,status_code) VALUES($1,1,'https://example.com/',$2,$3,'completed',200)`, other.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/"), cfg.TargetFingerprintKeyID); err != nil {
		t.Fatal(err)
	}
	w = extensionAPI(t, s, "GET", "/api/audits/"+other.RunID+"/landing-pages", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	if len(data.Items) != 0 {
		t.Fatal("data crossed audit boundary")
	}
}

func TestSearchInsightsUpgrade43PreservesReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || u.Host == "" {
		t.Fatal("DATABASE_URL required")
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "search44_" + strings.ReplaceAll(newWebRunID(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`)
	}()
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	goose.SetBaseFS(migrationFiles)
	goose.SetTableName(migrationVersionTable)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("pgx"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, db, migrationDir, 43); err != nil {
		t.Fatal(err)
	}
	id := newWebRunID()
	if _, err = db.ExecContext(ctx, `INSERT INTO audit_runs(id,started_at,heartbeat_at,worker_instance_id,status,total_urls) VALUES($1,NOW(),NOW(),'search-upgrade-test','running',0)`, id); err != nil {
		t.Fatal(err)
	}
	report := newWebRunID()
	if _, err = db.ExecContext(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count) VALUES($1,$2,$3,'example.com','local-lexical-v4',1,1,0,0)`, report, id, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err = db.QueryRowContext(ctx, `SELECT row_to_json(g)::text FROM geo_reports g WHERE id=$1`, report).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url) VALUES($1,1,'https://example.com/')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,title) VALUES($1,1,'https://example.com/', $2,'upgrade-fixture',200,'Historical title')`, id, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = applySchemaMigrationsDB(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if err = goose.DownToContext(ctx, db, migrationDir, 43); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT row_to_json(g)::text FROM geo_reports g WHERE id=$1`, report).Scan(&after); err != nil || before != after {
		t.Fatal("historical GEO report changed", err)
	}
	var title string
	if err = db.QueryRowContext(ctx, `SELECT title FROM audit_results WHERE run_id=$1 AND target_id=1`, id).Scan(&title); err != nil || title != "Historical title" {
		t.Fatal("historical SEO result changed", err)
	}
}

func TestSearchInsightsTenThousandRowsAreNotTruncated(t *testing.T) {
	_, s, cfg := searchFixture(t)
	base := "/api/audits/" + cfg.RunID
	var raw strings.Builder
	raw.WriteString(searchHeader)
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&raw, "2026-10-01,q%05d,https://example.com/products/a,US,desktop,1,0,3\n", i)
	}
	w := extensionAPI(t, s, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": raw.String()})
	if w.Code != 201 {
		t.Fatalf("10k import: %d %s", w.Code, w.Body.String())
	}
	w = extensionAPI(t, s, "GET", base+"/landing-pages?offset=9950", nil)
	if w.Code != 200 {
		t.Fatalf("10k last page: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []map[string]any
		More  bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 50 || result.More || result.Items[49]["query"] != "q09999" {
		t.Fatalf("last queries lost: %v", result)
	}
	if _, err := parseSearchCSV(context.Background(), cfg, "https://example.com", raw.String()+"2026-10-01,overflow,https://example.com/products/a,US,desktop,1,0,3\n"); err == nil {
		t.Fatal("10001st row was silently truncated")
	}
}

func TestLandingSummaryUsesPreviousObservedDayAndKeepsSegmentsSeparate(t *testing.T) {
	_, s, cfg := searchFixture(t)
	base := "/api/audits/" + cfg.RunID
	csv := searchHeader +
		"2026-10-01,history,https://example.com/products/a,US,desktop,100,0,3\n" +
		"2026-10-03,history,https://example.com/products/b,US,desktop,200,0,3\n" +
		"2026-10-05,history,https://example.com/products/a,US,desktop,50,0,3\n" +
		"2026-10-04,history,https://example.com/products/b,PL,mobile,80,0,3\n"
	w := extensionAPI(t, s, "POST", base+"/search-imports", map[string]any{"id": newWebRunID(), "csv": csv})
	if w.Code != 201 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	w = extensionAPI(t, s, "GET", base+"/landing-pages", nil)
	if w.Code != 200 {
		t.Fatalf("summary: %d %s", w.Code, w.Body.String())
	}
	var result struct{ Items []map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("segments: %v", result.Items)
	}
	for _, row := range result.Items {
		if row["device"] == "desktop" {
			if row["day"] != "2026-10-05" || row["previous_day"] != "2026-10-03" || row["previous_url"] != "https://example.com/products/b" || row["changed"] != true {
				t.Fatalf("previous observed day lost: %v", row)
			}
		} else if row["previous_day"] != nil || row["previous_url"] != nil || row["changed"] != nil {
			t.Fatalf("invented previous observation across segments: %v", row)
		}
	}
}
