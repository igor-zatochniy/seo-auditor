//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoverageRetainsURLsBeyondCrawlLimits(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var forbidden atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			http.NotFound(w, r)
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset><url><loc>http://%s/a</loc></url><url><loc>http://%s/b</loc></url><url><loc>http://%s/c</loc></url></urlset>`, r.Host, r.Host, r.Host)
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><title>Unchanged SEO</title></head><body><h1>Root heading</h1><a href="/deep?token=fixture-coverage-secret">Deep</a><a rel="nofollow" href="/nofollow">No</a></body>`)
		case "/a":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><title>A</title></head><body>Page A</body>`)
		default:
			forbidden.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	if e := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 2, MaxDepth: 0, UseSitemaps: true}); e != nil {
		t.Fatal(e)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	if forbidden.Load() != 0 {
		t.Fatalf("crawl bounds bypassed: %d", forbidden.Load())
	}
	app := extensionApp(cfg)
	app.pool = pool
	response := extensionAPI(t, app, "GET", "/api/audits/"+cfg.RunID+"/coverage", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "fixture-coverage-secret") {
		t.Fatal("URL secret leaked")
	}
	var report struct {
		Items []struct {
			URL, State string
			Sources    int
			Orphan     bool `json:"orphan_candidate"`
		}
		Summary map[string]int
		Meta    map[string]any
	}
	if e := json.Unmarshal(response.Body.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.Summary["known"] != 6 || report.Summary["sitemap"] != 3 || report.Summary["scanned"] != 2 {
		t.Fatalf("coverage=%s", response.Body.String())
	}
	if report.Meta["partial"] != true {
		t.Fatal("partial crawl claimed complete")
	}
	foundDepth, foundPage, foundNofollow := false, false, false
	for _, v := range report.Items {
		foundDepth = foundDepth || v.State == "depth_limit"
		foundPage = foundPage || v.State == "page_limit"
		foundNofollow = foundNofollow || v.State == "nofollow"
	}
	if !foundDepth || !foundPage || !foundNofollow {
		t.Fatal("missing reason", response.Body.String())
	}
	var title string
	if e := pool.QueryRow(ctx, `SELECT title FROM audit_results WHERE run_id=$1 AND target_id=1`, cfg.RunID).Scan(&title); e != nil || title != "Unchanged SEO" {
		t.Fatal("SEO regression", e, title)
	}
}

func TestCoverageImportsAreScopedIdempotentAndPrivate(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	if e := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: "https://example.com/", MaxPages: 10, MaxDepth: 1}); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=CURRENT_TIMESTAMP WHERE id=$1`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	app := extensionApp(cfg)
	app.pool = pool
	input := map[string]any{"id": newWebRunID(), "csv": "Page,Clicks\nhttps://example.com/catalog?token=fixture-AAAA,1\nhttps://example.com/catalog?token=fixture-BBBB,2", "important": true}
	path := "/api/audits/" + cfg.RunID + "/coverage/gsc"
	for _, expected := range []int{201, 200} {
		w := extensionAPI(t, app, "POST", path, input)
		if w.Code != expected {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var count int
	if e := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_crawl_inventory WHERE run_id=$1`, cfg.RunID).Scan(&count); e != nil || count != 3 {
		t.Fatal(e, count)
	}
	botID := newWebRunID()
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_crawl_source_imports WHERE bot_import_id=$1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_bot_imports WHERE id=$1`, botID)
	}()
	log := fmt.Sprintf(`{"request_uri":"https://example.com/catalog?token=fixture-AAAA","remote_addr":"66.249.66.1","user_agent":"Googlebot/2.1","status":200,"time":%q}`, time.Now().UTC().Format(time.RFC3339))
	w := extensionAPI(t, app, "POST", "/api/bots/imports", map[string]any{"id": botID, "domain": "example.com", "format": "jsonl", "log": log})
	if w.Code != 201 {
		t.Fatal("bot import", w.Code, w.Body.String())
	}
	source := map[string]any{"id": newWebRunID(), "import_id": botID}
	for _, expected := range []int{201, 200} {
		w = extensionAPI(t, app, "POST", "/api/audits/"+cfg.RunID+"/coverage/bots", source)
		if w.Code != expected {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = extensionAPI(t, app, "GET", "/api/audits/"+cfg.RunID+"/coverage", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "fixture-AAAA") || !strings.Contains(w.Body.String(), `"bot_visits": 1`) && !strings.Contains(w.Body.String(), `"bot_visits":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = extensionAPI(t, app, "GET", "/api/audits/"+cfg.RunID+"/facets", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"variants":2`) && !strings.Contains(w.Body.String(), `"variants": 2`) {
		t.Fatal("facets", w.Code, w.Body.String())
	}
	input["id"] = newWebRunID()
	input["csv"] = "URL\nhttps://foreign.example/page"
	w = extensionAPI(t, app, "POST", path, input)
	if w.Code != 400 {
		t.Fatal("foreign origin accepted")
	}
	if e := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_crawl_inventory WHERE run_id=$1`, cfg.RunID).Scan(&count); e != nil || count != 3 {
		t.Fatal("rejected import left partial state", e, count)
	}
}

func TestCustomRulesSnapshotAndResolvedAlerts(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, time.Minute)
	fixtureStatus := atomic.Int32{}
	fixtureStatus.Store(200)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(int(fixtureStatus.Load()))
		fmt.Fprint(w, `<head><title>Scheduled page</title></head><body>Content</body>`)
	}))
	defer fixture.Close()
	app := extensionApp(cfg)
	app.pool = pool
	input := scheduleInput{ID: newWebRunID(), Name: "Diagnostic rules fixture", Mode: "list", URLs: fixture.URL + "/product?token=rule-secret-AAAA", IntervalHours: 24, NextRunAt: time.Now().Add(time.Hour)}
	source, label, count, e := prepareSchedule(&input, cfg, 10)
	if e != nil {
		t.Fatal(e)
	}
	if e = app.storeSchedule(ctx, input, source, label, count); e != nil {
		t.Fatal(e)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_schedules WHERE id=$1`, input.ID) }()
	rule := customAuditRule{ID: newWebRunID(), Name: "HTTP правило <script>unsafe</script>", Pattern: "/product", Kind: "http_200", Severity: "critical", Enabled: true}
	w := extensionAPI(t, app, "POST", "/api/schedules/"+input.ID+"/rules", rule)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if e = json.Unmarshal(w.Body.Bytes(), &rule); e != nil {
		t.Fatal(e)
	}
	for index, status := range []int{200, 404, 200} {
		fixtureStatus.Store(int32(status))
		cfg.RunID = newWebRunID()
		if e = createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/product?token=rule-secret-AAAA"}); e != nil {
			t.Fatal(e)
		}
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		_, e = tx.Exec(ctx, `INSERT INTO audit_schedule_runs(run_id,schedule_id,key_check,scheduled_at) VALUES($1,$2,$3,CURRENT_TIMESTAMP)`, cfg.RunID, input.ID, fingerprintURL(cfg.TargetFingerprintKey, "schedule:"+input.ID))
		if e != nil {
			t.Fatal(e)
		}
		if e = captureAuditRules(ctx, tx, cfg, cfg.RunID, input.ID, true); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
			t.Fatal(code)
		}
		for attempts := 0; attempts < 20; attempts++ {
			done, e := app.processAuditRules(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if !done {
				break
			}
		}
		var state string
		e = pool.QueryRow(ctx, `SELECT state FROM audit_rule_results WHERE run_id=$1`, cfg.RunID).Scan(&state)
		if e != nil {
			t.Fatal(e)
		}
		expected := "pass"
		if status == 404 {
			expected = "fail"
		}
		if state != expected {
			t.Fatal(state, expected)
		}
		var alerts int
		e = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1`, cfg.RunID).Scan(&alerts)
		if e != nil || alerts != min(index, 1) {
			t.Fatal("alerts", index, alerts, e)
		}
		if index == 2 {
			var message string
			_ = pool.QueryRow(ctx, `SELECT after_value FROM audit_change_events WHERE run_id=$1`, cfg.RunID).Scan(&message)
			if !strings.Contains(message, "Виправлено") {
				t.Fatal(message)
			}
		}
	}
	exceptionPath := "/api/schedules/" + input.ID + "/rules/" + rule.ID + "/exceptions"
	w = extensionAPI(t, app, "POST", exceptionPath, map[string]any{"url": fixture.URL + "/product?token=rule-secret-AAAA", "reason": "Known temporary fixture failure", "expires_hours": 24})
	if w.Code != 200 {
		t.Fatalf("save exception: %d %s", w.Code, w.Body.String())
	}
	var savedException map[string]any
	if e = json.Unmarshal(w.Body.Bytes(), &savedException); e != nil {
		t.Fatal(e)
	}
	var safeURL string
	if e = pool.QueryRow(ctx, `SELECT safe_url FROM audit_rule_exceptions WHERE schedule_id=$1 AND rule_id=$2`, input.ID, rule.ID).Scan(&safeURL); e != nil || strings.Contains(safeURL, "rule-secret-AAAA") {
		t.Fatalf("exception persisted a raw secret URL: %q %v", safeURL, e)
	}
	w = extensionAPI(t, app, "GET", exceptionPath, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "rule-secret-AAAA") {
		t.Fatalf("exception list leaked the query value: %d %s", w.Code, w.Body.String())
	}
	w = extensionAPI(t, app, "DELETE", exceptionPath+"/"+savedException["fingerprint"].(string), nil)
	if w.Code != 200 {
		t.Fatalf("delete exception: %d %s", w.Code, w.Body.String())
	}
	oldRun := cfg.RunID
	rule.Revision = 1
	rule.Pattern = "/other/*"
	w = extensionAPI(t, app, "POST", "/api/schedules/"+input.ID+"/rules", rule)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var frozen []customAuditRule
	if e = pool.QueryRow(ctx, `SELECT rules FROM audit_rule_runs WHERE run_id=$1`, oldRun).Scan(&frozen); e != nil || frozen[0].Pattern != "/product" {
		t.Fatal("snapshot changed", e, frozen)
	}
	rule.Name = "Concurrent stale edit"
	w = extensionAPI(t, app, "POST", "/api/schedules/"+input.ID+"/rules", rule)
	if w.Code != 409 {
		t.Fatal("stale revision overwritten", w.Code, w.Body.String())
	}
	before := rule.ID
	rule.ID = newWebRunID()
	rule.Kind = "eval_sql"
	w = extensionAPI(t, app, "POST", "/api/schedules/"+input.ID+"/rules", rule)
	if w.Code != 400 {
		t.Fatal("execution accepted", before)
	}
}

func TestAuditRulesUseBoundedBatchesAndResumeAfterCancellation(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 2*time.Minute)
	input := scheduleInput{ID: newWebRunID(), Name: "Batched rule fixture", Mode: "list", URLs: "https://example.com/", IntervalHours: 24, NextRunAt: time.Now().Add(time.Hour)}
	app := extensionApp(cfg)
	app.pool = pool
	source, label, count, e := prepareSchedule(&input, cfg, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if e = app.storeSchedule(ctx, input, source, label, count); e != nil {
		t.Fatal(e)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_schedules WHERE id=$1`, input.ID) }()
	for i := 0; i < 32; i++ {
		rule := customAuditRule{ID: newWebRunID(), Revision: 1, Name: fmt.Sprintf("HTTP %d", i), Pattern: "/*", Kind: "http_200", Severity: "warning", Enabled: true}
		if _, e = pool.Exec(ctx, `INSERT INTO audit_custom_rules(id,schedule_id,definition) VALUES($1,$2,$3)`, rule.ID, input.ID, rule); e != nil {
			t.Fatal(e)
		}
	}
	urls := []string{}
	for i := 0; i < 1000; i++ {
		urls = append(urls, fmt.Sprintf("https://example.com/page/%d", i))
	}
	if e = createExplicitAuditRun(ctx, pool, &cfg, urls); e != nil {
		t.Fatal(e)
	}
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.CopyFrom(ctx, pgx.Identifier{"audit_results"}, []string{"run_id", "target_id", "safe_url", "target_fingerprint", "fingerprint_key_id", "status_code", "scan_status"}, pgx.CopyFromSlice(len(urls), func(i int) ([]any, error) {
		return []any{cfg.RunID, i + 1, urls[i], fingerprintURL(cfg.TargetFingerprintKey, urls[i]), cfg.TargetFingerprintKeyID, 200, "completed"}, nil
	})); e != nil {
		t.Fatal(e)
	}
	if e = captureAuditRules(ctx, tx, cfg, cfg.RunID, input.ID, false); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `UPDATE audit_run_targets SET status='completed',finished_at=CURRENT_TIMESTAMP WHERE run_id=$1`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `UPDATE audit_runs SET status='completed',successful_urls=1000,finished_at=CURRENT_TIMESTAMP WHERE id=$1`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, e = app.processAuditRules(canceled); e == nil {
		t.Fatal("cancellation ignored")
	}
	var cursor int
	if e = pool.QueryRow(ctx, `SELECT after_target FROM audit_rule_runs WHERE run_id=$1`, cfg.RunID).Scan(&cursor); e != nil || cursor != 0 {
		t.Fatal("cancellation changed cursor", cursor, e)
	}
	for pass := 0; pass < 20; pass++ {
		done, e := app.processAuditRules(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if !done {
			break
		}
	}
	var total int
	var state string
	if e = pool.QueryRow(ctx, `SELECT status,(SELECT COUNT(*) FROM audit_rule_results WHERE run_id=$1) FROM audit_rule_runs WHERE run_id=$1`, cfg.RunID).Scan(&state, &total); e != nil || state != "done" || total != 32000 {
		t.Fatal(state, total, e)
	}
	if done, e := app.processAuditRules(ctx); e != nil || done {
		t.Fatal("completed job replayed", done, e)
	}
}

func TestInventoryCapacityStillUpdatesKnownURLs(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 2*time.Minute)
	if e := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: "https://example.com/", MaxPages: 10, MaxDepth: 1}); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO audit_crawl_inventory(run_id,fingerprint,safe_url,sources) SELECT $1,decode(md5('fixture:'||v)||md5('fixture:'||v),'hex'),'https://example.com/fixture/'||v,2 FROM generate_series(1,49999) v`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `UPDATE audit_crawl_inventory_state SET url_count=50000 WHERE run_id=$1`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	old := inventoryURL(cfg, "https://example.com/", 4, 0)
	old.Important = true
	if e = storeInventory(ctx, tx, cfg.RunID, []inventoryRecord{old, inventoryURL(cfg, "https://example.com/new", 2, 0)}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	var limited, important bool
	if e = pool.QueryRow(ctx, `SELECT url_count,limited,(SELECT important FROM audit_crawl_inventory WHERE run_id=$1 AND fingerprint=$2) FROM audit_crawl_inventory_state WHERE run_id=$1`, cfg.RunID, old.Fingerprint).Scan(&count, &limited, &important); e != nil || count != 50000 || !limited || !important {
		t.Fatal(count, limited, important, e)
	}
	if e = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_crawl_inventory WHERE run_id=$1`, cfg.RunID).Scan(&count); e != nil || count != 50000 {
		t.Fatal(count, e)
	}
	app := extensionApp(cfg)
	app.pool = pool
	response := extensionAPI(t, app, "GET", "/api/audits/"+cfg.RunID+"/coverage", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var report struct {
		Items   []any
		Meta    map[string]any
		Summary map[string]int
	}
	if e = json.Unmarshal(response.Body.Bytes(), &report); e != nil || len(report.Items) != 50 || report.Summary["known"] != 50000 || report.Meta["inventory_limited"] != true {
		t.Fatal(e, len(report.Items), report.Summary, report.Meta)
	}
}
