//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/aicitation"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestConfirmedAIAnswerIsAtomicPrivateAndIdempotent(t *testing.T) {
	app, report, _ := visibilityReportFixture(t, 2*time.Minute)
	mux := app.handler()
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(string(encoded)))
		r.Header.Set("Authorization", "Bearer "+testWebToken)
		r.Header.Set("Origin", "http://127.0.0.1:8080")
		r.Header.Set("X-SEO-Auditor-Request", "1")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	base := fmt.Sprintf("/api/geo/reports/%s/queries/1", report.ID)
	input := aiAnswerInput{ID: newWebRunID(), Engine: "chatgpt", Answer: "Merchanto helps. <script>window.answerXss=1</script> Source https://example.com/report?token=answer-runtime-secret", Sources: "https://other.example/article"}
	preview := call("POST", base+"/answer/preview", input)
	if preview.Code != 200 || strings.Contains(preview.Body.String(), "answer-runtime-secret") {
		t.Fatal(preview.Code, preview.Body.String())
	}
	var a aicitation.Analysis
	if err := json.Unmarshal(preview.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Citations {
		if c.Relation == "own" {
			input.Selected = append(input.Selected, c.ID)
		}
	}
	if len(input.Selected) != 1 {
		t.Fatal(a)
	}
	w := call("POST", base+"/answer", input)
	if w.Code != 400 {
		t.Fatal("Не підтверджена відповідь збережена", w.Code)
	}
	input.Confirmed, input.BrandMention = true, true
	forged := input
	forged.Selected = []string{"unknown-citation"}
	if w = call("POST", base+"/answer", forged); w.Code != 400 {
		t.Fatal("Невідоме джерело прийнято", w.Code, w.Body.String())
	}
	var observations int
	if err := app.pool.QueryRow(t.Context(), "SELECT count(*) FROM geo_citation_observations WHERE id=$1", input.ID).Scan(&observations); err != nil || observations != 0 {
		t.Fatal("Відхилений аналіз створив observation", observations, err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- call("POST", base+"/answer", input).Code }()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 && code != 201 {
			t.Fatal("Concurrent idempotency", code)
		}
	}
	var count int
	var stored string
	if err := app.pool.QueryRow(t.Context(), "SELECT count(*),max(answer_text||analysis::text) FROM geo_ai_answers WHERE id=$1", input.ID).Scan(&count, &stored); err != nil || count != 1 || strings.Contains(stored, "answer-runtime-secret") {
		t.Fatal(count, stored, err)
	}
	var citation, mention string
	if err := app.pool.QueryRow(t.Context(), "SELECT citation,brand_mention FROM geo_citation_checks WHERE report_id=$1 AND ordinal=1", report.ID).Scan(&citation, &mention); err != nil || citation != "yes" || mention != "yes" {
		t.Fatal(citation, mention, err)
	}
	input.Answer += " Different answer"
	if w = call("POST", base+"/answer", input); w.Code != 409 {
		t.Fatal("ID reuse changed data", w.Code)
	}
	if w = call("GET", base+"/answers", nil); w.Code != 200 || strings.Contains(w.Body.String(), "answer-runtime-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", fmt.Sprintf("/api/geo/reports/%s/queries/1/answers", newWebRunID()), nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal("Інший звіт отримав дані", w.Code, w.Body.String())
	}
	unknown := aiAnswerInput{ID: newWebRunID(), Engine: "gemini", Answer: "An answer without readable sources", Confirmed: true}
	if w = call("POST", base+"/answer", unknown); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := app.pool.QueryRow(t.Context(), "SELECT citation FROM geo_citation_checks WHERE report_id=$1 AND ordinal=1 AND engine='gemini'", report.ID).Scan(&citation); err != nil || citation != "unknown" {
		t.Fatal("Невиявлене джерело помилково оголошено відсутнім", citation, err)
	}
}

func TestOptionalSemanticCorpusUsesFullIndexAndPreservesLexical(t *testing.T) {
	app, _, cfg := visibilityReportFixture(t)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, "DELETE FROM geo_corpora WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, `UPDATE audit_results SET title='Stripe payment protection',h1='Stripe payment protection' WHERE run_id=$1 AND target_id=1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	input := geo.Input{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Queries: "захист платежів Stripe", Semantic: true}
	queries, err := geo.Validate(&input)
	if err != nil {
		t.Fatal(err)
	}
	report, err := app.createGEO(ctx, input, queries)
	if err != nil {
		t.Fatal(err)
	}
	var result geo.Result
	if err = app.pool.QueryRow(ctx, "SELECT result FROM geo_query_results WHERE report_id=$1 AND ordinal=1", report.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if !report.Semantic || report.Model != geo.SemanticModel || result.TargetID == nil || *result.TargetID != 1 || result.Semantic == nil || !result.Semantic.Selected || result.Lexical == nil || result.Lexical.TargetID != nil {
		t.Fatalf("%+v %+v", report, result)
	}
	input.ID, input.Semantic = newWebRunID(), false
	plain, err := app.createGEO(ctx, input, queries)
	if err != nil {
		t.Fatal(err)
	}
	result = geo.Result{}
	if err = app.pool.QueryRow(ctx, "SELECT result FROM geo_query_results WHERE report_id=$1 AND ordinal=1", plain.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if plain.Semantic || plain.Model != geo.Model || result.Semantic != nil || result.TargetID != nil {
		t.Fatalf("Типовий режим змінився: %+v %+v", plain, result)
	}
}

func TestExtendedSiteDiscoveryAndDiskBackedGraph(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 2*time.Minute)
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: "https://example.com/", MaxPages: 10001, MaxDepth: 10}); err != nil {
		t.Fatal(err)
	}
	site, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for start := 2; start <= 10001; start += 100 {
		batch := []siteCandidate{}
		for i := start; i < start+100 && i <= 10001; i++ {
			batch = append(batch, siteCandidate{url: fmt.Sprintf("https://example.com/%d", i), depth: 1})
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = lockSiteOwner(ctx, tx, cfg); err == nil {
			err = addSiteTargets(ctx, tx, cfg, site, batch)
		}
		if err != nil {
			rollbackSiteTransaction(tx, cfg)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	if err = pool.QueryRow(ctx, "SELECT total_urls FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&total); err != nil || total != 10001 {
		t.Fatal(total, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockSiteOwner(ctx, tx, cfg); err == nil {
		err = addSiteTargets(ctx, tx, cfg, site, []siteCandidate{{url: "https://example.com/10001", depth: 1}, {url: "https://example.com/overflow", depth: 1}})
	}
	if err != nil {
		rollbackSiteTransaction(tx, cfg)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_site_edges(run_id,from_target_id,ordinal,to_fingerprint,safe_url,is_internal,nofollow,kind)
		SELECT $1,src.target_id,1,dst.url_fingerprint,dst.safe_url,TRUE,FALSE,CASE WHEN src.target_id=2 THEN 'redirect' ELSE 'link' END
		FROM audit_site_nodes src JOIN audit_site_nodes dst ON dst.run_id=src.run_id AND dst.target_id=src.target_id+1 WHERE src.run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	depths, err := siteDatabaseClickDepths(ctx, pool, cfg)
	if err != nil || len(depths) != 10001 || depths[10001] != 9999 {
		t.Fatal("Спрямований граф або нульова вартість redirect", len(depths), depths[10001], err)
	}
	if _, err = pool.Exec(ctx, "DELETE FROM audit_site_edges WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_site_edges(run_id,from_target_id,ordinal,to_fingerprint,safe_url,is_internal,nofollow,kind)
		SELECT $1,(n.target_id-2)/256+1,(n.target_id-2)%256+1,n.url_fingerprint,n.safe_url,TRUE,FALSE,'link' FROM audit_site_nodes n WHERE n.run_id=$1 AND n.target_id>1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	graphCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = finalizeSiteGraph(graphCtx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM audit_site_nodes WHERE run_id=$1 AND crawl_depth IS NOT NULL", cfg.RunID).Scan(&total); err != nil || total != 10001 {
		t.Fatal(total, err)
	}
}
