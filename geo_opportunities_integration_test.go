//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/pressly/goose/v3"
)

func TestGEOTargetDecisionsPreserveAutomaticResultsAndEnforceRevision(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	path := "/api/geo/reports/" + report.ID + "/queries/1/target"
	var original string
	if err := app.pool.QueryRow(t.Context(), "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	id := newWebRunID()
	body := fmt.Sprintf(`{"id":%q,"target_id":2,"expected_revision":0,"reason":"Перевірено https://example.com/?token=decision-secret"}`, id)
	w := strategyRequest(app, "POST", path, body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Revision < 1 {
		t.Fatal(response, err)
	}
	w = strategyRequest(app, "POST", path, body)
	if w.Code != 200 {
		t.Fatal("Повтор рішення не ідемпотентний", w.Code, w.Body.String())
	}
	for _, bad := range []string{
		strings.Replace(body, "\"target_id\":2", "\"target_id\":1", 1),
		strings.Replace(body, id, newWebRunID(), 1),
	} {
		if w = strategyRequest(app, "POST", path, bad); w.Code != 409 {
			t.Fatal("Конфлікт рішення прийнято", w.Code, w.Body.String())
		}
	}
	w = strategyRequest(app, "GET", "/api/geo/reports/"+report.ID, "")
	var view struct {
		Rows []geoRow `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || len(view.Rows) != 1 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	r := view.Rows[0]
	if r.MappingMode != "manual" || r.AutomaticResult == nil || r.Result.TargetID == nil || *r.Result.TargetID != 2 || r.DecisionRevision != response.Revision || strings.Contains(r.DecisionReason, "decision-secret") {
		t.Fatal(r)
	}
	var unchanged string
	if err := app.pool.QueryRow(t.Context(), "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&unchanged); err != nil || unchanged != original {
		t.Fatal("Автоматичний результат змінено", err)
	}
	var count int
	if err := app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("SEO-результати пошкоджено", count, err)
	}
	w = strategyRequest(app, "POST", path, fmt.Sprintf(`{"id":%q,"target_id":999,"expected_revision":%d,"reason":"Невідома ціль"}`, newWebRunID(), response.Revision))
	if w.Code != 400 {
		t.Fatal("Чужу ціль прийнято", w.Code, w.Body.String())
	}
	w = strategyRequest(app, "POST", path, fmt.Sprintf(`{"id":%q,"target_id":null,"expected_revision":%d,"reason":"Повернення до автоматичного вибору"}`, newWebRunID(), response.Revision))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = strategyRequest(app, "GET", "/api/geo/reports/"+report.ID, "")
	view.Rows = nil
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.Rows[0].MappingMode != "automatic" || view.Rows[0].AutomaticResult != nil {
		t.Fatal(view, err)
	}
	w = strategyRequest(app, "GET", path+"-history", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "decision-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := app.pool.Exec(t.Context(), "UPDATE geo_reports SET model='local-lexical-v3' WHERE id=$1", report.ID); err != nil {
		t.Fatal(err)
	}
	w = strategyRequest(app, "POST", path, fmt.Sprintf(`{"id":%q,"target_id":1,"expected_revision":0,"reason":"Стара модель"}`, newWebRunID()))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "model_changed") {
		t.Fatal("Змішані версії моделей", w.Code, w.Body.String())
	}
}

func TestGEOOpportunityExportAndImpactPreserveSource(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	w := strategyRequest(app, "GET", "/api/geo/reports/"+report.ID+"/opportunities", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var plan struct {
		Plan geo.OpportunityPlan `json:"plan"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || plan.Plan.Model != geo.OpportunityModel || plan.Plan.CheckedPages != 2 || len(plan.Plan.Warnings) == 0 {
		t.Fatal(plan, err)
	}
	w = strategyRequest(app, "GET", "/api/geo/reports/"+report.ID+"/targets", "")
	var targetPage struct {
		Targets []geo.PageFacts `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &targetPage); err != nil || w.Code != 200 || len(targetPage.Targets) != 2 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	w = strategyRequest(app, "GET", "/api/geo/reports/"+report.ID+"/tasks.csv", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Модель плану") {
		t.Fatal(w.Code, w.Body.String())
	}
	second, err := app.createGEO(ctx, geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: report.Domain, Brand: report.Brand}, []string{"chargeback protection"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/geo/reports/" + second.ID + "/impact?baseline=" + report.ID
	w = strategyRequest(app, "GET", path, "")
	var impact struct {
		Items []geo.ImpactChange `json:"items"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &impact); err != nil || w.Code != 200 || len(impact.Items) != 1 || !impact.Items[0].Comparable || impact.Items[0].RelevanceDelta == nil || *impact.Items[0].RelevanceDelta != 0 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if _, err = app.pool.Exec(ctx, "UPDATE geo_reports SET model='local-lexical-v3' WHERE id=$1", report.ID); err != nil {
		t.Fatal(err)
	}
	w = strategyRequest(app, "GET", path, "")
	if err = json.Unmarshal(w.Body.Bytes(), &impact); err != nil || w.Code != 200 || impact.Items[0].Comparable || impact.Items[0].RelevanceDelta != nil {
		t.Fatal(w.Code, w.Body.String(), err)
	}
}

func TestGEOTargetDecisionsSerializeConcurrentRevisions(t *testing.T) {
	app, report, _ := visibilityReportFixture(t)
	app.submissions = make(chan struct{}, 2)
	ctx := t.Context()
	lock, err := app.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(ctx, "SELECT id FROM geo_reports WHERE id=$1 FOR UPDATE", report.ID); err != nil {
		t.Fatal(err)
	}
	var original string
	if err = app.pool.QueryRow(ctx, "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	path := "/api/geo/reports/" + report.ID + "/queries/1/target"
	var responses [2]*httptest.ResponseRecorder
	var workers sync.WaitGroup
	for i := range responses {
		workers.Go(func() {
			body := fmt.Sprintf(`{"id":%q,"target_id":%d,"expected_revision":0,"reason":"Паралельне рішення"}`, newWebRunID(), i+1)
			responses[i] = strategyRequest(app, "POST", path, body)
		})
	}
	deadline := time.Now().Add(time.Second)
	for len(app.submissions) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	overlapped := len(app.submissions) == 2
	if err = lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if !overlapped {
		t.Fatal("Запити не дійшли одночасно до заблокованого звіту")
	}
	winner := -1
	for i, response := range responses {
		switch response.Code {
		case 201:
			if winner != -1 {
				t.Fatal("Прийнято два рішення з однаковою початковою ревізією")
			}
			winner = i
		case 409:
			if !strings.Contains(response.Body.String(), "stale_revision") {
				t.Fatal(response.Body.String())
			}
		default:
			t.Fatal(response.Code, response.Body.String())
		}
	}
	var count int
	var targetID int64
	if err = app.pool.QueryRow(ctx, "SELECT COUNT(*),MAX(target_id) FROM geo_target_decisions WHERE report_id=$1", report.ID).Scan(&count, &targetID); err != nil || count != 1 || targetID != int64(winner+1) {
		t.Fatal(count, targetID, err)
	}
	var unchanged string
	if err = app.pool.QueryRow(ctx, "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&unchanged); err != nil || unchanged != original {
		t.Fatal("Автоматичний результат змінено паралельним рішенням", err)
	}
}

func TestGEOVisibilitySeriesSelectsLatestImportForEachScope(t *testing.T) {
	app, report, _ := visibilityReportFixture(t)
	store := func(csv string) {
		t.Helper()
		input := geo.VisibilityInput{ID: newWebRunID(), Source: "ai_observed", Engine: "chatgpt", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", Country: "US", Device: "all", CSV: csv}
		rows, err := geo.ParseVisibilityCSV(&input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = app.storeVisibilityImport(t.Context(), report.ID, input, rows); err != nil {
			t.Fatal(err)
		}
	}
	get := func(scope string) []visibilityPoint {
		t.Helper()
		path := "/api/geo/reports/" + report.ID + "/visibility-series?ordinal=1&source=ai_observed&engine=chatgpt&country=US&device=all&scope=" + scope
		w := strategyRequest(app, "GET", path, "")
		var result struct {
			Points []visibilityPoint `json:"points"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Points) != 1 {
			t.Fatal(w.Code, w.Body.String(), err)
		}
		return result.Points
	}
	store("Query,Impressions\nchargeback protection,10\n")
	store("URL,Impressions\nhttps://example.com/report?token=alpha,42\n")
	points := get("query")
	if points[0].Impressions == nil || *points[0].Impressions != 10 {
		t.Fatal("URL-only імпорт приховав спостереження запиту", points)
	}
	points = get("url")
	if points[0].Impressions == nil || *points[0].Impressions != 42 {
		t.Fatal("URL-only спостереження втрачено", points)
	}
	store("Query,Impressions\nother query,99\n")
	if points = get("query"); points[0].Impressions != nil || points[0].Rows != 0 {
		t.Fatal("Відсутність запиту в останньому імпорті підмінено старим значенням", points)
	}
}

func TestGEOVisibilitySeriesDoesNotCrossFingerprintKeyRotation(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_ai", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", Country: "US", Device: "all", CSV: "URL,Impressions\nhttps://example.com/report?token=alpha,42\n"}
	rows, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.storeVisibilityImport(ctx, report.ID, input, rows); err != nil {
		t.Fatal(err)
	}
	cfg.RunID, cfg.TargetFingerprintKeyID = newWebRunID(), "rotated-test-key"
	cfg.TargetFingerprintKey = []byte("rotated-integration-only-fingerprint-key")
	if err = createExplicitAuditRun(ctx, app.pool, &cfg, []string{"https://example.com/report?token=alpha"}); err != nil {
		t.Fatal(err)
	}
	if err = finishStrategyFixture(ctx, app.pool, cfg, false); err != nil {
		t.Fatal(err)
	}
	current, err := app.createGEO(ctx, geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: report.Domain, Brand: report.Brand}, []string{"chargeback protection"})
	if err != nil {
		t.Fatal(err)
	}
	w := strategyRequest(app, "GET", "/api/geo/reports/"+current.ID+"/visibility-series?ordinal=1&source=gsc_ai&engine=google_ai&country=US&device=all&scope=url", "")
	var result struct {
		Points []visibilityPoint `json:"points"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Points) != 1 || result.Points[0].Impressions != nil || result.Points[0].Rows != 0 {
		t.Fatal("Старе спостереження приписано цілі після ротації ключа", w.Code, w.Body.String(), err)
	}
}

func TestGEOVisibilitySeriesSeparatesUnknownCountryAndURLIdentity(t *testing.T) {
	app, report, _ := visibilityReportFixture(t)
	store := func(start, end, country, csv, source string) {
		t.Helper()
		input := geo.VisibilityInput{ID: newWebRunID(), Source: source, Engine: "chatgpt", PeriodStart: start, PeriodEnd: end, Country: country, Device: "all", CSV: csv}
		rows, err := geo.ParseVisibilityCSV(&input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = app.storeVisibilityImport(t.Context(), report.ID, input, rows); err != nil {
			t.Fatal(err)
		}
	}
	store("2026-09-01", "2026-09-30", "US", "Query,Impressions,Clicks\nchargeback protection,0,0\n", "ai_observed")
	store("2026-09-10", "2026-09-11", "US", "Query,Impressions,Clicks\nchargeback protection,10,\n", "ai_observed")
	store("2026-09-20", "2026-09-21", "US", "Query,Impressions,Clicks\nother query,25,1\n", "ai_observed")
	store("2026-09-20", "2026-09-21", "UA", "Query,Impressions\nchargeback protection,999\n", "ai_observed")
	path := "/api/geo/reports/" + report.ID + "/visibility-series?ordinal=1&source=ai_observed&engine=chatgpt&country=US&device=all&scope=query"
	w := strategyRequest(app, "GET", path, "")
	var result struct {
		Points []visibilityPoint `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Points) != 3 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	p := result.Points
	if p[0].Impressions == nil || *p[0].Impressions != 0 || p[1].Clicks != nil || !p[1].Overlaps || p[2].Impressions != nil || !p[2].Overlaps {
		t.Fatal(p)
	}
	w = strategyRequest(app, "GET", strings.Replace(path, "ai_observed", "gsc_web", 1), "")
	if w.Code != 400 {
		t.Fatal("GSC Web потрапив у AI-граф", w.Code)
	}
	store("2026-10-01", "2026-10-02", "US", "URL,Impressions\nhttps://example.com/report?token=alpha,42\nhttps://example.com/report?token=beta,99\n", "gsc_ai")
	path = "/api/geo/reports/" + report.ID + "/visibility-series?ordinal=1&source=gsc_ai&engine=google_ai&country=US&device=all&scope=url"
	w = strategyRequest(app, "GET", path, "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Points) != 1 || result.Points[0].Impressions == nil || *result.Points[0].Impressions != 42 {
		t.Fatal("Колізія замаскованих URL у графіку", w.Code, w.Body.String(), err)
	}
	w = strategyRequest(app, "POST", "/api/geo/reports/"+report.ID+"/queries/1/target", fmt.Sprintf(`{"id":%q,"target_id":2,"expected_revision":0,"reason":"Перевірка другого signed URL"}`, newWebRunID()))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = strategyRequest(app, "GET", path, "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || *result.Points[0].Impressions != 99 {
		t.Fatal("Ручний вибір не враховано", w.Body.String(), err)
	}
}

func TestGEOOpportunityUpgradeFrom21PreservesHistory(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 21); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("SEO-історію втрачено", count, err)
	}
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("GEO-історію втрачено", count, err)
	}
	w := strategyRequest(app, "GET", "/api/geo/reports/"+report.ID+"/opportunities", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestGEOOpportunityGraphDoesNotRecommendExistingLinks(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/solutions", "https://example.com/blog"}); err != nil {
		t.Fatal(err)
	}
	if err := finishStrategyFixture(ctx, pool, cfg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_site_crawls(run_id,origin,root_safe_url,key_check,max_pages,max_depth,use_sitemaps,sitemap_state,graph_ready) VALUES($1,'https://example.com','https://example.com/solutions',$2,2,2,FALSE,'disabled',TRUE)`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "test-graph")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_site_nodes(run_id,target_id,url_fingerprint,safe_url,frontier_depth,internal_inlinks_count) SELECT run_id,target_id,target_fingerprint,safe_url,1,0 FROM audit_results WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	report, err := app.createGEO(ctx, geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Brand: "Merchanto"}, []string{"chargeback protection"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/geo/reports/" + report.ID + "/opportunities"
	var body struct {
		Plan geo.OpportunityPlan `json:"plan"`
	}
	w := strategyRequest(app, "GET", path, "")
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || len(body.Plan.Links) != 1 || body.Plan.Links[0].From != 2 || body.Plan.Links[0].To != 1 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_site_edges(run_id,from_target_id,ordinal,to_fingerprint,safe_url,is_internal,nofollow,kind) SELECT run_id,2,1,target_fingerprint,safe_url,TRUE,TRUE,'link' FROM audit_results WHERE run_id=$1 AND target_id=1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	w = strategyRequest(app, "GET", path, "")
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || len(body.Plan.Links) != 0 {
		t.Fatal("Наявне nofollow-посилання рекомендовано повторно", w.Code, w.Body.String(), err)
	}
}
