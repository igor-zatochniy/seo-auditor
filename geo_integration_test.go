//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

func TestGEONullBodyTextDoesNotLoseSEOResult(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<head><title>Перевірка SEO</title></head><body><h1>Головний заголовок</h1><p>до\x00після два слова</p><h2>Під\x00заголовок</h2></body>")
	}))
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("GEO зламав збереження SEO-результату: exit=%d", code)
	}
	var title, status string
	var wordCount int
	var signals *geo.Signals
	if err := pool.QueryRow(ctx, "SELECT title,scan_status,word_count,geo_signals FROM audit_results WHERE run_id=$1", cfg.RunID).
		Scan(&title, &status, &wordCount, &signals); err != nil {
		t.Fatal(err)
	}
	if title != "Перевірка SEO" || status != "completed" || wordCount == 0 || signals == nil || !signals.Complete {
		t.Fatalf("Втрачено SEO-дані: title=%q status=%s words=%d signals=%+v", title, status, wordCount, signals)
	}
	for _, text := range []string{signals.Excerpt, signals.Headings, signals.FirstParagraph} {
		if strings.ContainsRune(text, 0) || !strings.ContainsRune(text, '\uFFFD') {
			t.Fatal("Нульовий символ не замінено під час збереження GEO")
		}
	}
	var runStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&runStatus); err != nil || runStatus != "completed" {
		t.Fatalf("Звичайний SEO-аудит не завершився: %s %v", runStatus, err)
	}
}

func TestGEOStoredAuditMappingAndCitation(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := webFixture()
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("Код SEO-аудиту: %d", code)
	}
	var signals *geo.Signals
	if err := pool.QueryRow(ctx, "SELECT geo_signals FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&signals); err != nil || signals == nil || !signals.Complete {
		t.Fatalf("Сигнали не збережено: %+v %v", signals, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_results SET safe_url='https://example.com/page' WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Brand: "Приклад", Queries: "Integration H1\nbanana recipes"}
	queries, err := geo.Validate(&input)
	if err != nil {
		t.Fatal(err)
	}
	report, err := app.createGEO(ctx, input, queries)
	if err != nil || report.QueryCount != 2 || report.MatchedCount != 1 {
		t.Fatalf("Звіт: %+v %v", report, err)
	}
	// Повтор після втраченої HTTP-відповіді повертає той самий звіт.
	if duplicate, err := app.createGEO(ctx, input, queries); err != nil || duplicate.ID != report.ID {
		t.Fatalf("Повтор: %+v %v", duplicate, err)
	}
	if _, err := app.createGEO(ctx, input, []string{"different query"}); err == nil {
		t.Fatal("ID прийнято для іншого запиту")
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testWebToken)
		r.Header.Set("Origin", "http://127.0.0.1:8080")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-SEO-Auditor-Request", "1")
		w := httptest.NewRecorder()
		app.handler().ServeHTTP(w, r)
		return w
	}
	observationID := newWebRunID()
	observationBody := fmt.Sprintf(`{"id":%q,"engine":"chatgpt","citation":"yes","brand_mention":"yes","evidence_url":"https://example.com/share?token=private-value","note":"Ручний доказ"}`, observationID)
	endpoint := "/api/geo/reports/" + report.ID + "/queries/1/citation"
	w := request("POST", endpoint, observationBody)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-value") {
		t.Fatalf("Спостереження: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/api/geo/reports/"+report.ID, "")
	var result struct {
		Rows []geoRow `json:"rows"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Rows) != 2 || len(result.Rows[0].Citations) != 1 {
		t.Fatalf("API: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/api/geo/reports/"+report.ID+"/export/csv", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Ручний доказ") {
		t.Fatal("Ручне спостереження не експортовано")
	}
	// Повтор доставки не створює нове спостереження і не відновлює старий latest.
	if w = request("POST", endpoint, observationBody); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", endpoint, `{"engine":"chatgpt","citation":"no","brand_mention":"no","note":"Наступна перевірка"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", endpoint, observationBody); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", endpoint, strings.Replace(observationBody, "Ручний доказ", "Змінений доказ", 1)); w.Code != 409 {
		t.Fatal("ID спостереження перезаписано")
	}
	w = request("GET", "/api/geo/reports/"+report.ID+"/queries/1/observations", "")
	var history struct {
		Items []geoCitation `json:"items"`
		Next  int64         `json:"next"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history.Items) != 2 || history.Items[0].Note != "Наступна перевірка" || history.Items[1].ID != observationID {
		t.Fatalf("Історію втрачено: %s", w.Body.String())
	}
	w = request("GET", "/api/geo/reports/"+report.ID, "")
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Rows[0].Citations[0].Citation != "no" {
		t.Fatal("Повтор старого запиту перезаписав останню перевірку")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO geo_citation_observations(id,report_id,ordinal,engine,citation,brand_mention)
		SELECT gen_random_uuid(),$1,1,'chatgpt','unknown','unknown' FROM generate_series(1,55)`, report.ID); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/geo/reports/"+report.ID+"/queries/1/observations", "")
	if json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history.Items) != 50 || history.Next == 0 {
		t.Fatal("Історія не обмежена сторінкою")
	}
	w = request("GET", fmt.Sprintf("/api/geo/reports/%s/queries/1/observations?before=%d", report.ID, history.Next), "")
	if json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history.Items) != 7 || history.Next != 0 {
		t.Fatal("Пагінація загубила спостереження")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatal("GEO змінив результати SEO")
	}
}

func TestGEOThousandQueriesAtomicityAndLock(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_runs SET status='completed',finished_at=NOW() WHERE id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title)
		VALUES ($1,1,'https://example.com',decode(repeat('11',32),'hex'),'test',200,'completed','Stripe chargeback protection')`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	queries := make([]string, 1000)
	for i := range queries {
		queries[i] = fmt.Sprintf("Stripe chargeback %d", i)
	}
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com"}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310004)"); err != nil {
		t.Fatal(err)
	}
	if _, err = app.createGEO(ctx, input, queries); err == nil {
		t.Fatal("Паралельний GEO не заблоковано")
	}
	_ = tx.Rollback(ctx)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = app.createGEO(canceled, input, queries); err == nil {
		t.Fatal("Скасований запит збережено")
	}
	if _, err = app.createGEO(ctx, input, queries); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1", input.ID).Scan(&count); err != nil || count != 1000 {
		t.Fatalf("Кількість результатів: %d %v", count, err)
	}
	// Обмеження БД захищає верхню межу кількості результатів.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT 1 FROM geo_reports WHERE id=$1 FOR UPDATE", input.ID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, "INSERT INTO geo_query_results(report_id,ordinal,result) VALUES ($1,1001,'{}')", input.ID)
	if err == nil {
		t.Fatal("Ліміт рядків у БД не діє")
	}
	_ = tx.Rollback(ctx)
	input.ID = newWebRunID()
	if _, err = pool.Exec(ctx, `CREATE FUNCTION geo_test_reject_last() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.report_id='`+input.ID+`'::uuid AND NEW.ordinal=1000 THEN RAISE EXCEPTION 'test failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER geo_test_reject_last BEFORE INSERT ON geo_query_results FOR EACH ROW EXECUTE FUNCTION geo_test_reject_last()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS geo_test_reject_last ON geo_query_results; DROP FUNCTION IF EXISTS geo_test_reject_last()")
	})
	if _, err = app.createGEO(ctx, input, queries); err == nil {
		t.Fatal("Ін'єкція помилки не спрацювала")
	}
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_reports WHERE id=$1", input.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("Відмова на останньому рядку залишила частковий звіт")
	}
}

func TestGEOEmptySourceRollsBack(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com"}); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool = pool
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com"}
	if _, err := app.createGEO(ctx, input, []string{"query"}); err == nil {
		t.Fatal("Незавершене джерело прийнято")
	}
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_reports WHERE id=$1", input.ID).Scan(&count)
	if err != nil && err != pgx.ErrNoRows || count != 0 {
		t.Fatal("Частковий GEO-звіт залишився у БД")
	}
}
