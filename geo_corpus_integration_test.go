//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

func corpusTestSignals() *geo.Signals {
	return &geo.Signals{Version: 3, Complete: true, Search: geo.SearchControls{GooglebotRules: geo.Allowed, OAISearchBotRules: geo.Allowed, GoogleIndexing: geo.Allowed, GoogleSnippet: geo.Allowed}}
}

type cancelAfterCorpusCopy struct {
	pgx.Tx
	cancel context.CancelFunc
}

type observeCorpusCopy struct {
	pgx.Tx
	afterCopy func() error
}

func (tx *observeCorpusCopy) CopyFrom(ctx context.Context, name pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	count, err := tx.Tx.CopyFrom(ctx, name, columns, source)
	if err == nil && tx.afterCopy != nil {
		callback := tx.afterCopy
		tx.afterCopy = nil
		err = callback()
	}
	return count, err
}

func TestGEOCorpusUsesStableSnapshotAcrossBatches(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, "DELETE FROM geo_corpora WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) SELECT $1,i,'','completed',NOW() FROM generate_series(3,65) i`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title,h1,h1_count,geo_signals)
 SELECT $1,i,'https://example.com/page/'||i,decode(md5(i::text)||md5(i::text),'hex'),$2,200,'completed',CASE WHEN i=65 THEN 'Rare Stripe payment protection' ELSE 'Common payment information' END,'Common payment information',1,$3 FROM generate_series(3,65) i`, cfg.RunID, cfg.TargetFingerprintKeyID, corpusTestSignals()); err != nil {
		t.Fatal(err)
	}
	tx, err := app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	changed := false
	observed := &observeCorpusCopy{Tx: tx, afterCopy: func() error {
		changed = true
		_, err := app.pool.Exec(ctx, `UPDATE audit_results SET title='Unrelated replacement' WHERE run_id=$1 AND target_id=65`, cfg.RunID)
		return err
	}}
	corpus, err := ensureGEOCorpus(ctx, observed, cfg.RunID, report.Domain)
	if err != nil {
		t.Fatal(err)
	}
	result, err := corpus.chosen(ctx, "rare stripe payment protection", 65, "")
	if err != nil || !changed || corpus.summary.Pages != 65 || result.Coverage != 100 {
		t.Fatal("Пакети не використовують один snapshot", result, corpus.summary, changed, err)
	}
	var title string
	if err = app.pool.QueryRow(ctx, "SELECT title FROM audit_results WHERE run_id=$1 AND target_id=65", cfg.RunID).Scan(&title); err != nil || title != "Unrelated replacement" {
		t.Fatal("Конкурентна зміна не відбулася", title, err)
	}
}

func (tx cancelAfterCorpusCopy) CopyFrom(ctx context.Context, name pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	count, err := tx.Tx.CopyFrom(ctx, name, columns, source)
	if err == nil {
		tx.cancel()
	}
	return count, err
}

func TestGEOCorpusCancellationRollsBackWrittenBatch(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, "DELETE FROM geo_corpora WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	tx, err := app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = ensureGEOCorpus(work, cancelAfterCorpusCopy{tx, cancel}, cfg.RunID, report.Domain)
	if err == nil || work.Err() != context.Canceled {
		t.Fatal("Скасування після запису пакета не зупинило побудову", err)
	}
	if err = tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}
	var count int
	if err = app.pool.QueryRow(ctx, "SELECT count(*) FROM geo_corpora WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("Частковий індекс збережено після скасування", count, err)
	}
	if err = app.pool.QueryRow(ctx, "SELECT count(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("Історичний звіт змінено після скасування", count, err)
	}
}

func TestGEOCorpusMatchesResidentLexicalResults(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 3*time.Minute)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/root"}); err != nil {
		t.Fatal(err)
	}
	titles := []string{"Payment services", "Stripe integration", "Payment gateway", "Protection chargeback", "Chargeback protection", "Захист платежів", "Захист чарджбеків", "SKU12345 car chargeback", "Stripe payments", "Stripe payments", "Missing content"}
	pages := []geo.Page{}
	for i, title := range titles {
		id := int64(i + 1)
		if i == len(titles)-1 {
			id = -1
		}
		p := geo.Page{TargetID: id, URL: fmt.Sprintf("https://example.com/page/%d", id), Title: title, H1: title, H1Count: 1, Signals: corpusTestSignals()}
		p.Signals.Excerpt = "Payment facts"
		pages = append(pages, p)
		if _, err := pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) VALUES($1,$2,'','completed',NOW()) ON CONFLICT(run_id,target_id) DO UPDATE SET status='completed',request_url='',finished_at=NOW()`, cfg.RunID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title,h1,h1_count,geo_signals) VALUES($1,$2,$3,$4,$5,200,'completed',$6,$6,1,$7)`, cfg.RunID, id, p.URL, fingerprintURL(cfg.TargetFingerprintKey, p.URL), cfg.TargetFingerprintKeyID, title, p.Signals); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',total_urls=$2,successful_urls=$2,finished_at=NOW() WHERE id=$1`, cfg.RunID, len(pages)); err != nil {
		t.Fatal(err)
	}
	queries := []string{"payment stripe", "chargeback protection", "prevent chargebacks", "захист платежу", "захист чарджбеку", "chargebak protection", "sku12346", "cat", "zzchargeback", "stripe payments", "how what", "missing content"}
	want, err := geo.Analyze(ctx, queries, pages)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if want[i].TargetID != nil {
			for _, p := range pages {
				if p.TargetID == *want[i].TargetID {
					want[i].Entities = geo.EvaluateEntities("", p.Signals)
				}
			}
		}
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	corpus, err := ensureGEOCorpus(ctx, tx, cfg.RunID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	got, err := corpus.analyze(ctx, queries, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		for i := range got {
			if !reflect.DeepEqual(want[i], got[i]) {
				a, _ := json.Marshal(want[i])
				b, _ := json.Marshal(got[i])
				t.Errorf("%s\nresident=%s\npostgres=%s", queries[i], a, b)
			}
		}
	}
	if len(corpus.candidateCache) > geoCorpusBatchSize {
		t.Fatal("необмежений hydration cache")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM geo_corpora WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rollback індексу", count, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	app := testWebApp()
	app.pool, app.cfg = pool, cfg
	if _, err = app.createGEO(canceled, geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com"}, queries); !errors.Is(err, context.Canceled) {
		t.Fatal("скасування", err)
	}
}

func largeCorpusFixture(t *testing.T) (*webServer, Config) {
	t.Helper()
	ctx, pool, cfg := webIntegration(t, 5*time.Minute)
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/catalog/1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) SELECT $1,i,'','completed',NOW() FROM generate_series(2,10003) i`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status='completed',request_url='',finished_at=NOW() WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title,h1,h1_count,geo_signals)
 SELECT $1,i,CASE WHEN i=10002 THEN 'https://outside.test/rare' ELSE 'https://example.com/catalog/'||i END,decode(md5(i::text)||md5(i::text),'hex'),$2,
 CASE WHEN i=10003 THEN 404 ELSE 200 END,'completed',CASE WHEN i IN (10001,10002) THEN 'Rare Stripe chargeback protection' ELSE 'Common payments catalog' END,
 CASE WHEN i IN (10001,10002) THEN 'Rare Stripe chargeback protection' ELSE 'Common payments catalog' END,1,$3 FROM generate_series(1,10003) i`, cfg.RunID, cfg.TargetFingerprintKeyID, corpusTestSignals()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_results SET target_fingerprint=$2 WHERE run_id=$1 AND target_id=10001`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/catalog/10001")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed_with_errors',total_urls=10003,successful_urls=10002,failed_urls=1,finished_at=NOW() WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app := testWebApp()
	app.pool, app.cfg = pool, cfg
	return app, cfg
}

func TestGEOCorpusTenThousandPagesAndDownstreamPaths(t *testing.T) {
	app, cfg := largeCorpusFixture(t)
	ctx := t.Context()
	started := time.Now()
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Queries: "rare stripe chargeback protection"}
	report, err := app.createGEO(ctx, input, []string{input.Queries})
	if err != nil {
		t.Fatal(err)
	}
	if report.PageCount != 10001 || report.Corpus == nil || !report.Corpus.Complete || report.Corpus.SourcePages != 10002 || report.Corpus.Excluded != 1 {
		t.Fatal(report)
	}
	var original string
	if err = app.pool.QueryRow(ctx, "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var result geo.Result
	if json.Unmarshal([]byte(original), &result) != nil || result.TargetID == nil || *result.TargetID != 10001 || result.Matching.CorpusPages != 10001 {
		t.Fatal(original)
	}
	if repeated, err := app.createGEO(ctx, input, []string{input.Queries}); err != nil || repeated.ID != report.ID {
		t.Fatal("ідемпотентність", repeated, err)
	}
	path := "/api/geo/reports/" + report.ID
	comparisonInput := input
	comparisonInput.ID = newWebRunID()
	comparison, err := app.createGEO(ctx, comparisonInput, []string{input.Queries})
	if err != nil {
		t.Fatal(err)
	}
	impact := strategyRequest(app, "GET", "/api/geo/reports/"+comparison.ID+"/impact?baseline="+report.ID, "")
	var change struct {
		Items []geo.ImpactChange `json:"items"`
	}
	if err = json.Unmarshal(impact.Body.Bytes(), &change); err != nil || impact.Code != 200 || len(change.Items) != 1 || !change.Items[0].Comparable || change.Items[0].TargetChanged == nil || *change.Items[0].TargetChanged || change.Items[0].RelevanceDelta == nil || *change.Items[0].RelevanceDelta != 0 {
		t.Fatal("Порівняння має враховувати ідентичність цілі після 10000", impact.Code, impact.Body.String(), err)
	}
	w := strategyRequest(app, "GET", path+"/targets?search=10001", "")
	var selection struct {
		Targets []geo.PageFacts `json:"targets"`
		Next    *string         `json:"next_after"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &selection); err != nil || w.Code != 200 || len(selection.Targets) != 1 || selection.Targets[0].TargetID != 10001 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	w = strategyRequest(app, "GET", path+"/targets", "")
	if err = json.Unmarshal(w.Body.Bytes(), &selection); err != nil || w.Code != 200 || len(selection.Targets) != 50 || selection.Next == nil {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	w = strategyRequest(app, "GET", path+"/targets?after="+*selection.Next, "")
	if err = json.Unmarshal(w.Body.Bytes(), &selection); err != nil || w.Code != 200 || len(selection.Targets) != 50 || selection.Targets[0].TargetID != 51 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	w = strategyRequest(app, "POST", path+"/queries/1/target", fmt.Sprintf(`{"id":%q,"target_id":10001,"expected_revision":0,"reason":"Перевірено ціль наприкінці корпусу"}`, newWebRunID()))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var decision struct {
		Revision int64 `json:"revision"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &decision); err != nil || decision.Revision < 1 {
		t.Fatal("Ревізію рішення не отримано", err)
	}
	w = strategyRequest(app, "POST", path+"/queries/1/target", fmt.Sprintf(`{"id":%q,"target_id":10002,"expected_revision":0,"reason":"Інший домен"}`, newWebRunID()))
	if w.Code != 409 {
		t.Fatal("ревізія", w.Code, w.Body.String())
	}
	w = strategyRequest(app, "POST", path+"/queries/1/target", fmt.Sprintf(`{"id":%q,"target_id":10002,"expected_revision":%d,"reason":"Інший домен"}`, newWebRunID(), decision.Revision))
	if w.Code != 400 {
		t.Fatal("ізоляція домену", w.Code, w.Body.String())
	}
	otherCfg := cfg
	otherCfg.RunID = newWebRunID()
	if err = createExplicitAuditRun(ctx, app.pool, &otherCfg, []string{"https://example.com/foreign-run"}); err != nil {
		t.Fatal(err)
	}
	if _, err = app.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) VALUES($1,20000,'','completed',NOW());`, otherCfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = app.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title) VALUES($1,20000,'https://example.com/foreign-run',$2,$3,200,'completed','Rare Stripe chargeback protection')`, otherCfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/foreign-run"), cfg.TargetFingerprintKeyID); err != nil {
		t.Fatal(err)
	}
	w = strategyRequest(app, "POST", path+"/queries/1/target", fmt.Sprintf(`{"id":%q,"target_id":20000,"expected_revision":%d,"reason":"Інший запуск"}`, newWebRunID(), decision.Revision))
	if w.Code != 400 {
		t.Fatal("ізоляція запуску", w.Code, w.Body.String())
	}
	w = strategyRequest(app, "GET", path+"/opportunities", "")
	var plan struct {
		Plan geo.OpportunityPlan `json:"plan"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &plan); err != nil || w.Code != 200 || plan.Plan.CheckedPages != 10002 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	found := false
	for _, task := range plan.Plan.Tasks {
		if task.Kind == "technical" && task.TargetID != nil && *task.TargetID == 10003 {
			found = true
		}
	}
	if !found {
		t.Fatal("помилку наприкінці корпусу пропущено")
	}
	visibility := geo.VisibilityInput{ID: newWebRunID(), Source: "ai_observed", Engine: "google_ai", PeriodStart: "2026-10-01", PeriodEnd: "2026-10-10", Device: "all", CSV: "Query,URL,Impressions\nrare stripe chargeback protection,https://example.com/catalog/10001,7\n"}
	records, err := geo.ParseVisibilityCSV(&visibility)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.storeVisibilityImport(ctx, report.ID, visibility, records); err != nil || records[0].TargetID == nil || *records[0].TargetID != 10001 {
		t.Fatal(records, err)
	}
	var after string
	if err = app.pool.QueryRow(ctx, "SELECT result::text FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&after); err != nil || after != original {
		t.Fatal("автоматичний результат змінено", err)
	}
	var count int
	if err = app.pool.QueryRow(ctx, "SELECT count(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 10003 {
		t.Fatal("SEO-дані змінено", count, err)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("10001 domain pages: %s; live heap=%d, heap_sys=%d", time.Since(started), memory.HeapAlloc, memory.HeapSys)
	deleted := time.Now()
	var indexed bool
	if err = app.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND indexname='geo_corpus_pages_result_idx')`).Scan(&indexed); err != nil || !indexed {
		t.Fatal("Відсутній індекс FK великого corpus", indexed, err)
	}
	if err = cleanupWebIntegrationFixtures(app.pool, cfg.WorkerInstanceID); err != nil {
		t.Fatal("Пакетне видалення власного великого corpus", err)
	}
	if err = app.pool.QueryRow(ctx, "SELECT count(*) FROM geo_corpora WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("Залишився похідний індекс видаленого fixture run", count, err)
	}
	t.Logf("10001-page bounded cleanup: %s", time.Since(deleted))
}

func TestGEOCorpusDenseMatchesAvoidRepeatedProfileScans(t *testing.T) {
	app, cfg := largeCorpusFixture(t)
	ctx := t.Context()
	tx, err := app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	corpus, err := ensureGEOCorpus(ctx, tx, cfg.RunID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	query := "common payments catalog"
	prepared, err := geo.PrepareIndexedQuery(ctx, query, corpus.summary.Pages, corpus)
	if err != nil {
		t.Fatal(err)
	}
	variants, err := json.Marshal(prepared.Variants)
	if err != nil {
		t.Fatal(err)
	}
	var plan json.RawMessage
	if err = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+geoCandidateSQL, cfg.RunID, "example.com", geoIndexModel, variants, prepared.TotalIDF, prepared.PhraseKeys).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	type planNode struct {
		NodeType     string     `json:"Node Type"`
		RelationName string     `json:"Relation Name"`
		IndexName    string     `json:"Index Name"`
		Plans        []planNode `json:"Plans"`
	}
	var plans []struct{ Plan planNode }
	if err = json.Unmarshal(plan, &plans); err != nil {
		t.Fatal(err)
	}
	indexed := false
	var checkPlan func(planNode)
	checkPlan = func(node planNode) {
		if node.RelationName == "geo_corpus_terms" && node.NodeType == "Seq Scan" {
			t.Fatal("Ranking не повинен читати всі postings без статистики нового corpus", string(plan))
		}
		indexed = indexed || node.IndexName == "geo_corpus_terms_rank_idx"
		for _, child := range node.Plans {
			checkPlan(child)
		}
	}
	for _, root := range plans {
		checkPlan(root.Plan)
	}
	if !indexed {
		t.Fatal("Не використано індекс термінів corpus", string(plan))
	}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now()
	for range 30 {
		candidates, err := corpus.rank(check, query)
		if err != nil || len(candidates) != 3 || candidates[0].Page.TargetID != 1 || candidates[2].Page.TargetID != 3 {
			t.Fatal("Повний dense ranking", candidates, err, string(plan))
		}
	}
	t.Logf("30 full-corpus dense queries: %s", time.Since(started))
}

func TestGEOCorpusUpgradeFrom23PreservesHistory(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	before := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/results", "")
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 23); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	after := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/results", "")
	if before.Code != 200 || after.Code != 200 || before.Body.String() != after.Body.String() {
		t.Fatal("історичний SEO-звіт змінено")
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT count(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	newReport, err := app.createGEO(t.Context(), geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com"}, []string{"chargeback protection"})
	if err != nil || newReport.Corpus == nil || newReport.PageCount != 2 {
		t.Fatal(newReport, err)
	}
}

func TestWebIntegrationCleanupUsesBoundedCorpusBatches(t *testing.T) {
	app, cfg := largeCorpusFixture(t)
	ctx := t.Context()
	report, err := app.createGEO(ctx, geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com"}, []string{"rare stripe chargeback protection"})
	if err != nil {
		t.Fatal(err)
	}
	// Синтетичний fan-out перевіряє cleanup, не точність 1000 зіставлень.
	if _, err = app.pool.Exec(ctx, `INSERT INTO geo_query_results(report_id,ordinal,result)
		SELECT report_id,i,jsonb_set(result,'{query}',to_jsonb('cleanup fixture query '||i))
		FROM geo_query_results CROSS JOIN generate_series(2,1000) i WHERE report_id=$1 AND ordinal=1`, report.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = app.pool.Exec(ctx, `UPDATE geo_reports SET query_count=1000,matched_count=1000 WHERE id=$1`, report.ID); err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.RunID = newWebRunID()
	other.WorkerInstanceID += "-other"
	if err := createExplicitAuditRun(ctx, app.pool, &other, []string{"https://example.com/untouched"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanupWebIntegrationFixtures(app.pool, other.WorkerInstanceID); err != nil {
			t.Error(err)
		}
	})
	if err := cleanupWebIntegrationFixtures(app.pool, cfg.WorkerInstanceID); err != nil {
		t.Fatal(err)
	}
	var ownExists, otherExists bool
	if err := app.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_runs WHERE id=$1),EXISTS(SELECT 1 FROM audit_runs WHERE id=$2)`, cfg.RunID, other.RunID).Scan(&ownExists, &otherExists); err != nil || ownExists || !otherExists {
		t.Fatal("cleanup має видаляти лише власні fixtures", ownExists, otherExists, err)
	}
	if err := cleanupWebIntegrationFixtures(app.pool, cfg.WorkerInstanceID); err != nil {
		t.Fatal("повторний cleanup має бути ідемпотентним", err)
	}
}

func TestGEOCorpusIndexedLinksSearchPastIneligibleTopCandidates(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) SELECT $1,i,'','completed',NOW() FROM generate_series(3,7) i`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title,h1,h1_count,geo_signals)
 SELECT $1,i,'https://example.com/source/'||i,decode(md5(i::text)||md5(i::text),'hex'),$2,200,'completed','Chargeback protection','Chargeback protection',1,$3 FROM generate_series(3,7) i`, cfg.RunID, cfg.TargetFingerprintKeyID, corpusTestSignals()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, "DELETE FROM geo_corpora WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `UPDATE audit_results SET geo_signals=jsonb_set(geo_signals,'{complete}','false') WHERE run_id=$1 AND target_id BETWEEN 3 AND 5`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	tx, err := app.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	corpus, err := ensureGEOCorpus(ctx, tx, cfg.RunID, report.Domain)
	if err != nil {
		t.Fatal(err)
	}
	var original geo.Result
	if err = tx.QueryRow(ctx, "SELECT result FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// Фікстура має query-параметри на початкових цілях; рекомендація використовує безпечний URL.
	original.TargetURL = "https://example.com/target"
	original.Eligibility = &geo.AIEligibility{}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_site_crawls(run_id,origin,root_safe_url,key_check,max_pages,max_depth,use_sitemaps) VALUES($1,'https://example.com','https://example.com/target',$2,10,2,false)`, cfg.RunID, []byte(strings.Repeat("x", 32))); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_site_nodes(run_id,target_id,url_fingerprint,safe_url,frontier_depth,internal_inlinks_count) SELECT run_id,target_id,target_fingerprint,safe_url,0,0 FROM audit_results WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	links, limited, err := corpus.internalLinks(ctx, geo.PlanInput{Results: []geo.Result{original}, GraphFull: true})
	if err != nil || limited || len(links) != 2 || links[0].From != 6 || links[1].From != 7 {
		t.Fatal(links, limited, err)
	}
}
