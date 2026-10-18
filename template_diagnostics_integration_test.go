//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"strings"
	"testing"
	"time"
)

func TestPageTypeCDNAndRemediationFlow(t *testing.T) {
	ctx, s, cfg := searchFixture(t)
	base := "/api/audits/" + cfg.RunID
	c := structureddata.NewCollector()
	c.AddBlock([]byte(`{"@type":"Product","offers":{"price":10,"availability":"https://schema.org/InStock"}}`))
	_, err := s.pool.Exec(ctx, `UPDATE audit_results SET structured_data=$2,title=$3,meta_robots='noindex',x_robots_tag='googlebot: noindex',rendering=$4 WHERE run_id=$1 AND target_id=2`, cfg.RunID, c.Result("https://example.com/products/a"), "<script>untrusted</script> https://example.com/?token=package-secret", map[string]any{"status": "completed", "rendered": map[string]any{"title": "DOM title", "canonical": "https://example.com/?token=dom-secret", "complete": true}})
	if err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err = s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY target_id)::text FROM audit_results a WHERE run_id=$1`, cfg.RunID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w := extensionAPI(t, s, "GET", base+"/page-types", nil)
	if w.Code != 200 {
		t.Fatalf("types %d %s", w.Code, w.Body.String())
	}
	var types struct {
		Items   []typedPage
		Summary map[string]int
	}
	if err = json.Unmarshal(w.Body.Bytes(), &types); err != nil || types.Summary["product"] != 2 {
		t.Fatalf("%+v %v", types, err)
	}
	for _, p := range types.Items {
		if p.ID == 2 && typeState(p, "product_availability") != "observed" {
			t.Fatalf("%+v", p)
		}
	}
	payload := map[string]any{"origin": "https://example.com", "revision": 0, "rules": []pageTypeRule{{"/products/*", "category"}}}
	w = extensionAPI(t, s, "POST", base+"/page-types/rules", payload)
	if w.Code != 200 {
		t.Fatalf("rules %d %s", w.Code, w.Body.String())
	}
	if w = extensionAPI(t, s, "POST", base+"/page-types/rules", payload); w.Code != 409 {
		t.Fatal("lost update accepted", w.Code)
	}
	w = extensionAPI(t, s, "POST", base+"/results/2/page-type", map[string]any{"type": "article", "revision": 0, "reason": "Перевірено вручну"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = extensionAPI(t, s, "GET", base+"/page-types?type=article", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &types); err != nil || len(types.Items) != 1 || types.Items[0].Source != "manual" {
		t.Fatalf("%s %v", w.Body.String(), err)
	}
	if w = extensionAPI(t, s, "GET", base+"/page-types?origin=https://foreign.example", nil); w.Code != 400 {
		t.Fatal("foreign origin accepted", w.Code)
	}
	if w = extensionAPI(t, s, "POST", base+"/results/99999/page-type", map[string]any{"type": "article", "revision": 0, "reason": "test"}); w.Code != 404 {
		t.Fatal("foreign target accepted", w.Code)
	}
	importID := newWebRunID()
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(clean, `DELETE FROM audit_bot_imports WHERE id=$1`, importID)
	})
	log := ""
	for i := 0; i < 2; i++ {
		log += fmt.Sprintf(`{"url":"https://example.com/products/b?token=fixture-secret","client_ip":"203.0.113.3","user_agent":"OAI-SearchBot","method":"GET","status":429,"origin_status":200,"cache_status":"HIT","request_id":"r%d","timestamp":%q}`, i, time.Now().UTC().Format(time.RFC3339Nano)) + "\n"
	}
	body := map[string]any{"id": importID, "domain": "example.com", "format": "fastly", "log": log}
	for i := 0; i < 2; i++ {
		w = extensionAPI(t, s, "POST", "/api/bots/imports", body)
		if w.Code != 201 && w.Code != 200 {
			t.Fatalf("CDN %d %s", w.Code, w.Body.String())
		}
	}
	var events int
	if err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_bot_events WHERE import_id=$1 AND origin_status=200 AND cache_status='hit' AND octet_length(event_fingerprint)=32`, importID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events %d %v", events, err)
	}
	w = extensionAPI(t, s, "GET", base+"/cdn-diagnostics?import_id="+importID, nil)
	if w.Code != 200 {
		t.Fatalf("CDN report %d %s", w.Code, w.Body.String())
	}
	var cdn struct{ Items []map[string]any }
	if err = json.Unmarshal(w.Body.Bytes(), &cdn); err != nil || len(cdn.Items) != 1 || cdn.Items[0]["audit_discrepancy"] != true || cdn.Items[0]["target_id"] != float64(3) || cdn.Items[0]["repeated_429_503"] != true {
		t.Fatalf("CDN=%s %v", w.Body.String(), err)
	}
	if strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "203.0.113.3") {
		t.Fatal("CDN privacy leak")
	}
	w = extensionAPI(t, s, "GET", "/api/bots/imports/"+importID, nil)
	if w.Code != 200 {
		t.Fatalf("bot compatibility %s", w.Body.String())
	}
	target := int64(2)
	report := geoReport{ID: newWebRunID(), SourceRunID: cfg.RunID, Domain: "example.com", Model: "local-lexical-v4"}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	plan := geo.OpportunityPlan{Model: "fixture", CheckedPages: 3, Tasks: []geo.Opportunity{{ID: "fixture", TargetID: &target, TargetURL: "https://example.com/products/a", Action: "Перевірити індексування", Kind: "technical", Priority: "high"}}}
	pkg, err := buildRemediationPackage(ctx, tx, report, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Tasks) != 1 || pkg.Tasks[0].Proof == nil || pkg.Tasks[0].Proof.HTML["meta_robots"] != "noindex" || pkg.Tasks[0].Proof.DOM["title"] != "DOM title" || pkg.Tasks[0].Proof.ObservedAt.IsZero() {
		t.Fatalf("package=%+v", pkg)
	}
	raw, _ := json.Marshal(pkg)
	if strings.Contains(string(raw), "package-secret") || strings.Contains(string(raw), "dom-secret") {
		t.Fatalf("export privacy leak %s", raw)
	}
	if !strings.Contains(packageMarkdown(pkg), "googlebot: noindex") || strings.Contains(packageMarkdown(pkg), "<script>") {
		t.Fatal("unsafe or missing exact directive")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY target_id)::text FROM audit_results a WHERE run_id=$1`, cfg.RunID).Scan(&after); err != nil || before != after {
		t.Fatal("source SEO data changed", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count) VALUES($1,$2,$3,'example.com','local-lexical-v4',1,3,1,0)`, report.ID, cfg.RunID, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO geo_query_results(report_id,ordinal,result) VALUES($1,1,$2)`, report.ID, geo.Result{Query: "test query", TargetID: &target, TargetURL: "https://example.com/products/a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"md", "json"} {
		w = extensionAPI(t, s, "GET", "/api/geo/reports/"+report.ID+"/tasks."+format, nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s %d %s", format, w.Code, w.Body.String())
		}
	}
}

func TestPageTypesFullCorpusAndPagination(t *testing.T) {
	ctx, s, cfg := searchFixture(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) SELECT $1,n,'','completed',NOW() FROM generate_series(4,1500) n`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status)
 SELECT $1,n,'https://example.com/blog/page-'||n,decode(md5(n::text)||md5(n::text),'hex'),$2,200,'completed' FROM generate_series(4,1500) n`, cfg.RunID, cfg.TargetFingerprintKeyID)
	if err != nil {
		t.Fatal(err)
	}
	w := extensionAPI(t, s, "GET", "/api/audits/"+cfg.RunID+"/page-types?type=article&offset=1450", nil)
	var data struct {
		Checked, Matched int
		Items            []typedPage
		More             bool
	}
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil || data.Checked != 1500 || data.Matched != 1497 || len(data.Items) != 47 || data.More || data.Items[46].ID != 1500 {
		t.Fatalf("full corpus lost: %+v %v", data, err)
	}
}

func TestPageTypesIncludeLegacyTargetsAndSafeRecheck(t *testing.T) {
	ctx, s, cfg := searchFixture(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at) VALUES($1,-7,'','completed',NOW())`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title) VALUES($1,-7,'https://example.com/blog/legacy',$2,'legacy',200,'completed','Historical article')`, cfg.RunID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	base := "/api/audits/" + cfg.RunID
	w := extensionAPI(t, s, "GET", base+"/page-types", nil)
	var data struct {
		Checked int
		Items   []typedPage
	}
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || data.Checked != 4 || len(data.Items) != 4 || data.Items[0].ID != -7 || data.Items[0].Type != "article" {
		t.Fatalf("legacy target omitted: %s %v", w.Body.String(), err)
	}
	w = extensionAPI(t, s, "POST", base+"/results/-7/page-type", map[string]any{"type": "service", "revision": 0, "reason": "Уточнення історичної сторінки"})
	if w.Code != 200 {
		t.Fatalf("legacy override: %d %s", w.Code, w.Body.String())
	}
	w = extensionAPI(t, s, "GET", base+"/page-types?type=service", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || len(data.Items) != 1 || data.Items[0].ID != -7 || data.Items[0].Source != "manual" {
		t.Fatalf("legacy override not applied: %s %v", w.Body.String(), err)
	}
	target := int64(-7)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackSiteTransaction(tx, cfg)
	pkg, err := buildRemediationPackage(ctx, tx, geoReport{SourceRunID: cfg.RunID}, geo.OpportunityPlan{Tasks: []geo.Opportunity{{TargetID: &target, TargetURL: "https://example.com/blog/legacy"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Tasks) != 1 || pkg.Tasks[0].Proof == nil || pkg.Tasks[0].Recheck["endpoint"] != nil || !strings.Contains(pkg.Tasks[0].Recheck["instruction"].(string), "Історичний target") {
		t.Fatalf("invalid legacy recheck: %+v", pkg.Tasks)
	}
}

func TestCDNParentImportKeepsOriginsIsolated(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	urls := []string{"https://www.example.com/product", "https://shop.example.com/product", "https://example.net/product"}
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	for i, u := range urls {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status) VALUES($1,$2,$3,$4,$5,200,'completed')`, cfg.RunID, i+1, u, fingerprintURL(cfg.TargetFingerprintKey, u), cfg.TargetFingerprintKeyID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=NOW(),successful_urls=3 WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	s := extensionApp(cfg)
	s.pool = pool
	id := newWebRunID()
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(clean, `DELETE FROM audit_bot_imports WHERE id=$1`, id)
	})
	var raw strings.Builder
	for i, u := range urls {
		fmt.Fprintf(&raw, "{\"url\":%q,\"method\":\"GET\",\"user_agent\":\"OAI-SearchBot\",\"status\":403,\"request_id\":\"r%d\",\"timestamp\":%q}\n", u, i, time.Now().UTC().Format(time.RFC3339Nano))
	}
	w := extensionAPI(t, s, "POST", "/api/bots/imports", map[string]any{"id": id, "domain": "example.com", "format": "fastly", "log": raw.String()})
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	w = extensionAPI(t, s, "GET", "/api/audits/"+cfg.RunID+"/cdn-diagnostics?origin=https://www.example.com&import_id="+id, nil)
	var data struct{ Items []map[string]any }
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || len(data.Items) != 1 || data.Items[0]["url"] != urls[0] || data.Items[0]["target_id"] != float64(1) {
		t.Fatalf("origin leaked: %s %v", w.Body.String(), err)
	}
	if w = extensionAPI(t, s, "GET", "/api/audits/"+cfg.RunID+"/cdn-diagnostics?origin=https://example.net&import_id="+id, nil); w.Code != 404 {
		t.Fatal("foreign import accepted", w.Code, w.Body.String())
	}
}
