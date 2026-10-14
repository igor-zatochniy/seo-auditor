//go:build integration

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/pressly/goose/v3"
)

func visibilityReportFixture(t *testing.T, budget ...time.Duration) (*webServer, geoReport, Config) {
	t.Helper()
	ctx, pool, cfg := webIntegration(t, budget...)
	urls := []string{"https://example.com/report?token=alpha", "https://example.com/report?token=beta"}
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	signals := &geo.Signals{Version: 3, Complete: true, Excerpt: "Merchanto chargeback protection", FirstParagraph: "Chargeback protection", Entities: &geo.EntitySample{Version: 1, Complete: true, Organizations: []geo.EntityRecord{{Name: "Merchanto"}}}}
	for i, raw := range urls {
		_, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,robots_outcome,title,h1,h1_count,geo_signals)
			VALUES($1,$2,$3,$4,$5,200,'completed','allowed','Chargeback protection','Chargeback protection',1,$6)`, cfg.RunID, i+1, redactURL(raw), fingerprintURL(cfg.TargetFingerprintKey, raw), cfg.TargetFingerprintKeyID, signals)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_run_targets SET status='completed',finished_at=NOW(),request_url='' WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_runs SET status='completed',successful_urls=2,finished_at=NOW() WHERE id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Brand: "Merchanto", Queries: "chargeback protection"}
	queries, err := geo.Validate(&input)
	if err != nil {
		t.Fatal(err)
	}
	report, err := app.createGEO(ctx, input, queries)
	if err != nil {
		t.Fatal(err)
	}
	return app, report, cfg
}

func TestGEOVisibilityImportIsAtomicIdempotentAndCollisionSafe(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "ai_observed", Engine: "chatgpt", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", Country: "US", Device: "all",
		CSV: "Query,URL,Impressions,Clicks\nchargeback protection,https://example.com/report?token=alpha,1420,38\nchargeback protection,https://example.com/report?token=beta,0,0\n"}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	created, err := app.storeVisibilityImport(ctx, report.ID, input, records)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].TargetID == nil || records[1].TargetID == nil || *records[0].TargetID == *records[1].TargetID || records[0].URL != records[1].URL || records[0].ReportOrdinal == nil || records[1].ReportOrdinal != nil {
		t.Fatalf("Колізія або неправильна прив'язка: %+v", records)
	}
	var serialized string
	if err = app.pool.QueryRow(ctx, "SELECT jsonb_agg(observation)::text FROM geo_visibility_rows WHERE import_id=$1", created.ID).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serialized, "alpha") || strings.Contains(serialized, "beta") || strings.Contains(serialized, "RequestURL") {
		t.Fatal("Сирий URL потрапив до імпорту")
	}
	if duplicate, err := app.storeVisibilityImport(ctx, report.ID, input, records); err != nil || duplicate.ID != created.ID {
		t.Fatalf("Повтор UUID: %+v %v", duplicate, err)
	}
	input.ID = newWebRunID()
	fresh, _ := geo.ParseVisibilityCSV(&input)
	if duplicate, err := app.storeVisibilityImport(ctx, report.ID, input, fresh); err != nil || duplicate.ID != created.ID {
		t.Fatalf("Повтор файла: %+v %v", duplicate, err)
	}
	input.ID = newWebRunID()
	input.CSV = "URL,Impressions\nhttps://outside.example/report,1\n"
	input.Source = "gsc_ai"
	fresh, err = geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.storeVisibilityImport(ctx, report.ID, input, fresh); err == nil {
		t.Fatal("Чужий домен прийнято")
	}
	var count int
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_visibility_imports WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Незавершений імпорт залишив дані: %d %v", count, err)
	}
	input.ID = newWebRunID()
	input.CSV = "Page,Impressions\nhttps://example.com/report?token=beta,1420\n"
	fresh, err = geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.storeVisibilityImport(ctx, report.ID, input, fresh); err != nil || fresh[0].Clicks != nil || fresh[0].ReportOrdinal != nil || fresh[0].Query != "" {
		t.Fatalf("Вигадано AI-кліки чи запит: %+v %v", fresh, err)
	}
	input.ID = newWebRunID()
	input.Source = "gsc_web"
	input.CSV = "URL,Impressions\nhttps://EXAMPLE.com/report,1\nhttps://example.com/report,1\n"
	fresh, err = geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.storeVisibilityImport(ctx, report.ID, input, fresh); err == nil {
		t.Fatal("Нормалізовані дублікати URL прийнято")
	}
	var result geo.Result
	if err = app.pool.QueryRow(ctx, "SELECT result FROM geo_query_results WHERE report_id=$1 AND ordinal=1", report.ID).Scan(&result); err != nil || result.Entities == nil || result.Entities.Checks[1].Status != "found" {
		t.Fatalf("Сутності не збережено: %+v %v", result, err)
	}
	_ = cfg
}

func TestGEOVisibilityAPIPaginationAndDeletionPreserveAudit(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	var csv strings.Builder
	csv.WriteString("Query,Impressions\n")
	for i := range 205 {
		fmt.Fprintf(&csv, "test query %d,1\n", i)
	}
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_web", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", CSV: csv.String()}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	created, err := app.storeVisibilityImport(t.Context(), report.ID, input, records)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testWebToken)
		r.Header.Set("Origin", "http://127.0.0.1:8080")
		r.Header.Set("X-SEO-Auditor-Request", "1")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.handler().ServeHTTP(w, r)
		return w
	}
	path := "/api/geo/reports/" + report.ID + "/visibility/imports/" + created.ID
	for _, tc := range []struct{ after, count, next int }{{0, 100, 100}, {100, 100, 200}, {200, 5, 0}} {
		w := request("GET", fmt.Sprintf("%s?after=%d", path, tc.after), "")
		var result struct {
			Rows []geo.VisibilityRecord `json:"rows"`
			Next int                    `json:"next"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Rows) != tc.count || result.Next != tc.next {
			t.Fatalf("Пагінація: %d %s %v", w.Code, w.Body.String(), err)
		}
	}
	if w := request("POST", path+"/delete", "{}"); w.Code != 200 {
		t.Fatalf("Видалення: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("Видалення імпорту пошкодило SEO")
	}
	if w := request("GET", path, ""); w.Code != 404 {
		t.Fatal("Імпорт не видалено")
	}
}

func TestGEOVisibilityMigrationFrom18PreservesSourceData(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 18); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("Upgrade 18→19 пошкодив GEO")
	}
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("Upgrade 18→19 пошкодив SEO")
	}
}

func TestImpactAnalyticsSeparatesGSCGA4AndAIImpressions(t *testing.T) {
	app, report, _ := visibilityReportFixture(t)
	ctx := t.Context()
	importCSV := func(source, start, end, csv string) string {
		t.Helper()
		input := geo.VisibilityInput{ID: newWebRunID(), Source: source, PeriodStart: start, PeriodEnd: end, Country: "US", Device: "all", CSV: csv}
		records, err := geo.ParseVisibilityCSV(&input)
		if err != nil {
			t.Fatal(err)
		}
		created, err := app.storeVisibilityImport(ctx, report.ID, input, records)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	baseline := importCSV("gsc_web", "2026-08-01", "2026-08-31", "Page,Clicks,Impressions,CTR,Position\nhttps://example.com/report?token=alpha,500,10000,5%,4\nhttps://example.com/report?token=beta,50,1000,5%,4\n")
	web := importCSV("gsc_web", "2026-09-01", "2026-09-30", "Page,Clicks,Impressions,CTR,Position\nhttps://example.com/report?token=alpha,10,1000,1%,4\nhttps://example.com/report?token=beta,15,300,5%,4\n")
	ai := importCSV("gsc_ai", "2026-09-01", "2026-09-30", "Page,Impressions\nhttps://example.com/report?token=alpha,700\nhttps://example.com/report?token=beta,300\n")
	ga4 := importCSV("ga4", "2026-09-01", "2026-09-30", "Landing page,Session source,Sessions\n/report?token=alpha,chatgpt.com / referral,12\n/report?token=beta,example.com / referral,5\n")
	request := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/geo/reports/"+report.ID+"/impact-analytics?gsc_web="+web+"&gsc_ai="+ai+"&ga4="+ga4, nil)
	request.Header.Set("Authorization", "Bearer "+testWebToken)
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	request.Header.Set("X-SEO-Auditor-Request", "1")
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	var result struct {
		Baseline *impactDataset `json:"baseline"`
		Items    []impactPageDB `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 {
		t.Fatalf("impact API: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	if result.Baseline == nil || result.Baseline.ID != baseline || len(result.Items) != 2 {
		t.Fatalf("baseline/pages: %+v count=%d", result.Baseline, len(result.Items))
	}
	first := result.Items[0]
	if first.TargetID != 1 || first.URLMatch != "exact" || first.GSCImpressions == nil || *first.GSCImpressions != 1000 || first.GSCClicks == nil || *first.GSCClicks != 10 || first.GSCCTR == nil || math.Abs(*first.GSCCTR-.01) > 1e-9 {
		t.Fatalf("GSC Web metrics: %+v", first)
	}
	if first.GSC_AIImpressions == nil || *first.GSC_AIImpressions != 700 || first.AIReferralSessions == nil || *first.AIReferralSessions != 12 || first.GA4Sessions == nil || *first.GA4Sessions != 12 {
		t.Fatalf("AI/GА4 metrics mixed or missing: %+v", first)
	}
	if first.OpportunityClicks == nil || math.Abs(*first.OpportunityClicks-40) > 1e-9 || first.OpportunityConfidence != "low" {
		t.Fatalf("opportunity estimate: %+v", first)
	}
	if result.Items[1].AIReferralSessions != nil || result.Items[1].GA4Sessions == nil || *result.Items[1].GA4Sessions != 5 {
		t.Fatalf("non-AI referral was classified as AI: %+v", result.Items[1])
	}
	var stored string
	if err := app.pool.QueryRow(ctx, "SELECT jsonb_agg(observation)::text FROM geo_visibility_rows WHERE import_id=$1", ga4).Scan(&stored); err != nil || strings.Contains(stored, "token=alpha") || strings.Contains(stored, "token=beta") {
		t.Fatalf("raw GA4 query parameter leaked to storage: %s err=%v", stored, err)
	}
}

func TestImpactAnalyticsMigrationDownUpPreservesGA4Import(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	id := newWebRunID()
	if _, err := app.pool.Exec(ctx, `INSERT INTO geo_visibility_imports
		(id,report_id,payload_hash,source,engine,period_start,period_end,country,device,row_count)
		VALUES($1,$2,$3,'ga4','other','2026-09-01','2026-09-30','US','all',1)`, id, report.ID, fingerprintURL(cfg.TargetFingerprintKey, "ga4-migration-fixture")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `INSERT INTO geo_visibility_rows(import_id,ordinal,observation)
		VALUES($1,1,'{"url":"https://example.com/","sessions":3}')`, id); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(ctx, db, migrationDir, 40); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_visibility_rows WHERE import_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback deleted GA4 data: rows=%d err=%v", count, err)
	}
	if err = applySchemaMigrations(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_visibility_rows WHERE import_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade did not preserve GA4 data: rows=%d err=%v", count, err)
	}
}
