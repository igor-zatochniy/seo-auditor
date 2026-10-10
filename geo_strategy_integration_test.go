//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func finishStrategyFixture(ctx context.Context, pool *pgxpool.Pool, cfg Config, blocked bool) error {
	rules := geo.Allowed
	if blocked {
		rules = geo.Blocked
	}
	signals := &geo.Signals{Version: 3, Complete: true, Excerpt: "Merchanto chargeback protection", FirstParagraph: "Chargeback protection", Search: geo.SearchControls{GooglebotRules: geo.Allowed, OAISearchBotRules: rules, GoogleIndexing: geo.Allowed, GoogleSnippet: geo.Allowed}, HTTP: &geo.HTTPObservation{Status: 200}, Entities: &geo.EntitySample{Version: 1, Complete: true, Organizations: []geo.EntityRecord{{Name: "Merchanto"}}}}
	if blocked {
		signals.Entities.Organizations = nil
	}
	rows, err := pool.Query(ctx, "SELECT target_id,request_url FROM audit_run_targets WHERE run_id=$1", cfg.RunID)
	if err != nil {
		return err
	}
	var targets []targetURLRecord
	for rows.Next() {
		var v targetURLRecord
		if err = rows.Scan(&v.ID, &v.URL); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range targets {
		_, err = pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,robots_outcome,title,h1,h1_count,geo_signals)
		VALUES($1,$2,$3,$4,$5,200,'completed','allowed','Chargeback protection','Chargeback protection',1,$6)`, cfg.RunID, v.ID, redactURL(v.URL), fingerprintURL(cfg.TargetFingerprintKey, v.URL), cfg.TargetFingerprintKeyID, signals)
		if err != nil {
			return err
		}
	}
	if _, err = pool.Exec(ctx, "UPDATE audit_run_targets SET status='completed',finished_at=NOW(),request_url='' WHERE run_id=$1", cfg.RunID); err != nil {
		return err
	}
	_, err = pool.Exec(ctx, "UPDATE audit_runs SET status='completed',successful_urls=total_urls,finished_at=NOW() WHERE id=$1", cfg.RunID)
	return err
}
func strategyRequest(app *webServer, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testWebToken)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("X-SEO-Auditor-Request", "1")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.handler().ServeHTTP(w, r)
	return w
}
func cleanupStrategy(t *testing.T, app *webServer) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, table := range []string{"geo_competitor_jobs", "geo_competitor_sets", "geo_profile_changes", "geo_profile_runs", "geo_profiles"} {
			_, _ = app.pool.Exec(ctx, "DELETE FROM "+table)
		}
	})
}

func TestGEOProfilesCaptureScheduledRunsAndPreserveHistory(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	app, scheduleID := integrationSchedule(t, ctx, pool, cfg, "https://example.com/page")
	web := testWebApp()
	app.web, app.queries, app.submissions = web.web, web.queries, web.submissions
	cleanupStrategy(t, app)
	input := geoProfileInput{ID: newWebRunID(), ScheduleID: scheduleID, Name: "Профіль бренду", Domain: "example.com", Brand: "Merchanto", Queries: "chargeback protection\nStripe integration"}
	if err := app.storeGEOProfile(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := app.storeGEOProfile(ctx, input); err != nil {
		t.Fatal("Ідемпотентність:", err)
	}
	bad := input
	bad.ID = newWebRunID()
	bad.Domain = "outside.example"
	if err := app.storeGEOProfile(ctx, bad); err == nil {
		t.Fatal("Чужий домен профілю прийнято")
	}
	blocked := false
	app.manager.execute = func(c context.Context, p *pgxpool.Pool, v Config, _ bool) int {
		if err := finishStrategyFixture(c, p, v, blocked); err != nil {
			t.Error(err)
			return exitFatal
		}
		return exitSuccess
	}
	first, err := app.manager.startScheduled(ctx, scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, app.manager)
	if _, err := app.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.processGEOProfileJob(ctx); err != nil || !ok {
		t.Fatalf("Перший job: %v %v", ok, err)
	}
	var firstReport string
	if err = pool.QueryRow(ctx, "SELECT report_id::text FROM geo_profile_runs WHERE profile_id=$1 AND run_id=$2 AND status='ready'", input.ID, first).Scan(&firstReport); err != nil {
		var details string
		_ = pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_object('profile',j.profile_id,'run',j.run_id,'status',j.status,'error',j.error_message,'source_status',r.status))::text FROM geo_profile_runs j JOIN audit_runs r ON r.id=j.run_id").Scan(&details)
		t.Fatalf("%v first=%s profile=%s jobs=%s", err, first, input.ID, details)
	}
	blocked = true
	second, err := app.manager.startScheduled(ctx, scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, app.manager)
	if _, err := app.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.processGEOProfileJob(ctx); err != nil || !ok {
		t.Fatalf("Другий job: %v %v", ok, err)
	}
	var baseline string
	var count int
	if err = pool.QueryRow(ctx, "SELECT baseline_report_id::text FROM geo_profile_runs WHERE profile_id=$1 AND run_id=$2", input.ID, second).Scan(&baseline); err != nil || baseline != firstReport {
		t.Fatalf("baseline=%s %v", baseline, err)
	}
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_profile_changes WHERE profile_id=$1 AND run_id=$2 AND kind='oai_searchbot_rules'", input.ID, second).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Подія OAI: %d %v", count, err)
	}
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_profile_changes WHERE profile_id=$1 AND run_id=$2 AND kind='entity:organization' AND before_value='found' AND after_value='missing'", input.ID, second).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Подія Organization: %d %v", count, err)
	}
	var secondReport string
	if err = pool.QueryRow(ctx, "SELECT report_id::text FROM geo_profile_runs WHERE profile_id=$1 AND run_id=$2", input.ID, second).Scan(&secondReport); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ id, start, end, csv string }{{firstReport, "2026-09-01", "2026-09-30", "URL,Impressions\nhttps://example.com/page,0\n"}, {secondReport, "2026-10-01", "2026-10-09", "URL,Impressions\nhttps://example.com/page,42\n"}} {
		in := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_ai", PeriodStart: v.start, PeriodEnd: v.end, Country: "US", Device: "all", CSV: v.csv}
		records, err := geo.ParseVisibilityCSV(&in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = app.storeVisibilityImport(ctx, v.id, in, records); err != nil {
			t.Fatal(err)
		}
	}
	w := strategyRequest(app, "GET", "/api/geo/profiles/"+input.ID+"/observations?run_id="+second, "")
	var observed struct {
		Items []observedVisibilityChange `json:"items"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &observed); err != nil || w.Code != 200 || len(observed.Items) != 1 || observed.Items[0].Scope != "url" || observed.Items[0].Query != "" || observed.Items[0].After != 42 {
		t.Fatalf("AI-спостереження: %d %s %v", w.Code, w.Body.String(), err)
	}
	if ok, err := app.processGEOProfileJob(ctx); err != nil || ok {
		t.Fatalf("Повтор створив звіт: %v %v", ok, err)
	}
	for _, path := range []string{"/api/geo/profiles", "/api/geo/profiles/" + input.ID + "/history", "/api/geo/profiles/" + input.ID + "/timeline", "/api/geo/profiles/" + input.ID + "/changes?run_id=" + second, "/api/geo/profiles/" + input.ID + "/observations?run_id=" + second, "/api/geo/reports/" + firstReport + "/topics"} {
		if w := strategyRequest(app, "GET", path, ""); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if _, err = pool.Exec(ctx, "UPDATE geo_profiles SET enabled=FALSE WHERE id=$1", input.ID); err != nil {
		t.Fatal(err)
	}
	third, err := app.manager.startScheduled(ctx, scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, app.manager)
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_profile_runs WHERE run_id=$1", third).Scan(&count); err != nil || count != 0 {
		t.Fatal("Призупинений профіль створив job", count, err)
	}
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", first).Scan(&count); err != nil || count != 1 {
		t.Fatal("Старі SEO-дані змінено")
	}
}

func TestGEOCompetitorsReuseFreshAuditsAndBoundNewScans(t *testing.T) {
	app, parent, cfg := visibilityReportFixture(t)
	cleanupStrategy(t, app)
	ctx := t.Context()
	app.manager = newAuditManager(ctx, app.pool, cfg)
	app.manager.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	t.Cleanup(app.manager.shutdown)
	for _, raw := range []string{"https://first.example/page", "https://second.example/page"} {
		v := cfg
		v.RunID = newWebRunID()
		if err := createExplicitAuditRun(ctx, app.pool, &v, []string{raw}); err != nil {
			t.Fatal(err)
		}
		if err := finishStrategyFixture(ctx, app.pool, v, false); err != nil {
			t.Fatal(err)
		}
	}
	input := competitorSetInput{ID: newWebRunID(), Competitors: []competitorInput{{URL: "https://first.example/page"}, {URL: "https://second.example/page"}}}
	if err := app.storeCompetitorSet(ctx, parent.ID, input); err != nil {
		t.Fatal(err)
	}
	if err := app.storeCompetitorSet(ctx, parent.ID, input); err != nil {
		t.Fatal(err)
	}
	app.manager.execute = func(context.Context, *pgxpool.Pool, Config, bool) int {
		t.Error("Свіжий аудит запущено повторно")
		return exitFatal
	}
	for range 2 {
		if _, err := app.manager.startCompetitor(ctx); err != nil {
			t.Fatal(err)
		}
		if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
			t.Fatalf("Reuse: %v %v", ok, err)
		}
	}
	var reused int
	if err := app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND reused AND status='ready'", input.ID).Scan(&reused); err != nil || reused != 2 {
		t.Fatal(reused, err)
	}
	var competitorReport string
	if err := app.pool.QueryRow(ctx, "SELECT report_id::text FROM geo_competitor_jobs WHERE set_id=$1 AND ordinal=1", input.ID).Scan(&competitorReport); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ id, citation string }{{parent.ID, "no"}, {competitorReport, "yes"}} {
		if _, err := app.pool.Exec(ctx, "INSERT INTO geo_citation_checks(report_id,ordinal,engine,citation,brand_mention) VALUES($1,1,'chatgpt',$2,'unknown')", v.id, v.citation); err != nil {
			t.Fatal(err)
		}
	}
	if w := strategyRequest(app, "GET", "/api/geo/competitors/"+input.ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"gaps"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := strategyRequest(app, "GET", "/api/geo/competitors/"+input.ID, ""); !strings.Contains(w.Body.String(), "Ручне AI-цитування · ChatGPT") {
		t.Fatal("Citation gap не показано", w.Body.String())
	}
	other := competitorSetInput{ID: newWebRunID(), MaxPages: 7, Competitors: []competitorInput{{URL: "third.example"}, {URL: "fourth.example"}}}
	if err := app.storeCompetitorSet(ctx, parent.ID, other); err != nil {
		t.Fatal(err)
	}
	started := make(chan Config, 1)
	release := make(chan struct{})
	app.manager.execute = func(c context.Context, p *pgxpool.Pool, v Config, _ bool) int {
		started <- v
		select {
		case <-release:
		case <-c.Done():
			return exitCanceled
		}
		if err := finishStrategyFixture(c, p, v, false); err != nil {
			t.Error(err)
			return exitFatal
		}
		return exitSuccess
	}
	runID, err := app.manager.startCompetitor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := <-started
	if v.RenderJavaScript {
		t.Fatal("Конкурентний crawl увімкнув JS")
	}
	if _, err = app.manager.startCompetitor(ctx); err != errAuditBusy {
		t.Fatal("Другий crawl не заблокований", err)
	}
	var pages, depth int
	var sitemap bool
	if err = app.pool.QueryRow(ctx, "SELECT max_pages,max_depth,use_sitemaps FROM audit_site_crawls WHERE run_id=$1", runID).Scan(&pages, &depth, &sitemap); err != nil || pages != 7 || depth != 2 || sitemap {
		t.Fatal(pages, depth, sitemap, err)
	}
	close(release)
	waitScheduled(t, app.manager)
	if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if w := strategyRequest(app, "POST", "/api/geo/competitors/"+other.ID+"/cancel", "{}"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var canceled int
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND status='canceled'", other.ID).Scan(&canceled); err != nil || canceled != 1 {
		t.Fatal(canceled, err)
	}
	var count int
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", parent.SourceRunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("Старий SEO-звіт пошкоджено")
	}
}

func TestGEOStrategyUpgradeFrom19PreservesSEOAndGEO(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 19); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, query := range []string{"SELECT COUNT(*) FROM audit_results WHERE run_id=$1", "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1"} {
		id := cfg.RunID
		expected := 2
		if strings.Contains(query, "report_id") {
			id = report.ID
			expected = 1
		}
		if err = app.pool.QueryRow(t.Context(), query, id).Scan(&n); err != nil || n != expected {
			t.Fatal(n, err)
		}
	}
	var items []map[string]any
	w := strategyRequest(app, "GET", "/api/geo/profiles", "")
	if err = json.Unmarshal(w.Body.Bytes(), &items); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
}
