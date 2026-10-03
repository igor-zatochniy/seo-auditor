//go:build integration

package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSiteGraphAtConfiguredBudget(t *testing.T) {
	_, pool, cfg := webIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	setupStart := time.Now()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: "https://example.com/", MaxPages: maxSitePages, MaxDepth: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url) SELECT $1,g,'https://example.com/'||g FROM generate_series(2,1000) g`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_site_nodes(run_id,target_id,url_fingerprint,safe_url,frontier_depth)
		SELECT $1,g,decode(lpad(to_hex(g),64,'0'),'hex'),'https://example.com/'||g,1 FROM generate_series(2,1000) g`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_site_edges(run_id,from_target_id,ordinal,to_fingerprint,safe_url,is_internal,nofollow,kind)
		SELECT $1,src.target_id,g,dst.url_fingerprint,dst.safe_url,TRUE,FALSE,'link'
		FROM audit_site_nodes src CROSS JOIN generate_series(1,256) g
		JOIN audit_site_nodes dst ON dst.run_id=$1 AND dst.target_id=(src.target_id+g)%1000+1 WHERE src.run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	t.Logf("large fixture prepared in %s", time.Since(setupStart))
	graphCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := finalizeSiteGraph(graphCtx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	t.Logf("1000 nodes / 256000 edges finalized in %s", time.Since(start))
	var reachable int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=$1 AND crawl_depth IS NOT NULL`, cfg.RunID).Scan(&reachable); err != nil || reachable != 1000 {
		t.Fatalf("reachable=%d err=%v", reachable, err)
	}
}

func TestSiteSitemapIndexGzipAndScope(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var outsideHits atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outsideHits.Add(1); http.NotFound(w, r) }))
	defer external.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprintf(w, "User-agent: *\nDisallow: /denied.xml\nSitemap: %s/index.xml\nSitemap: %s/leave.xml\nSitemap: %s/denied.xml\n", base, base, base)
		case "/index.xml":
			fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/child.xml.gz</loc></sitemap></sitemapindex>`, base)
		case "/child.xml.gz":
			z := gzip.NewWriter(w)
			fmt.Fprintf(z, `<urlset><url><loc>%s/sitemap-only</loc></url></urlset>`, base)
			z.Close()
		case "/leave.xml":
			http.Redirect(w, r, external.URL+"/map.xml", 302)
		case "/denied.xml":
			t.Error("robots-disallowed sitemap fetched")
			http.NotFound(w, r)
		case "/sitemap.xml":
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<body>Site</body>")
		}
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 10, MaxDepth: 3, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	if outsideHits.Load() != 0 {
		t.Fatal("sitemap left origin scope")
	}
	var count int
	var state string
	if err := pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=$1),sitemap_state FROM audit_site_crawls WHERE run_id=$1`, cfg.RunID).Scan(&count, &state); err != nil || count != 2 || state != "partial" {
		t.Fatalf("count=%d state=%s err=%v", count, state, err)
	}
}

func TestSiteCrawlPipelineGraphAndPrivacy(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var mu sync.Mutex
	hits := map[string]int{}
	var order []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.RequestURI()]++
		order = append(order, r.URL.Path)
		mu.Unlock()
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nDisallow: /blocked\nSitemap: "+base+"/map.xml\n")
			return
		case "/sitemap.xml":
			http.NotFound(w, r)
			return
		case "/map.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset><url><loc>%s/orphan</loc></url><url><loc>%s/</loc></url></urlset>`, base, base)
			return
		case "/gone":
			http.NotFound(w, r)
			return
		case "/move":
			http.Redirect(w, r, "/final", 301)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<title>Test page</title><body>")
		switch r.URL.Path {
		case "/":
			io.WriteString(w, `<a href="/a">A</a><a href="/a#fragment">Again</a><a href="/gone">Broken</a><a href="/move">Moved</a><a href="/blocked">Blocked</a><a href="/signed?token=alpha">Alpha</a><a href="/signed?token=beta">Beta</a><a href="/nofollow" rel="nofollow">No follow</a><a href="https://example.com/out">External</a><template><a href="/hidden">Hidden</a></template>`)
		case "/a":
			io.WriteString(w, `<a href="/b">B</a><a href="/">Root</a>`)
		case "/b":
			io.WriteString(w, `<a href="/c">C</a>`)
		case "/c":
			io.WriteString(w, `<a href="/d">D</a>`)
		}
		io.WriteString(w, "</body>")
	}))
	defer server.Close()
	options := siteCrawlOptions{RootURL: server.URL, MaxPages: 50, MaxDepth: 5, UseSitemaps: true}
	if err := createSiteAuditRun(ctx, pool, &cfg, options); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	run, err := loadWebRun(ctx, pool, cfg, cfg.RunID)
	if err != nil || run.Mode != "site" || run.Status != auditRunStatusCompleted || run.Total != 12 {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	var pending, retained int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE status IN ('pending','running')),COUNT(*) FILTER(WHERE request_url<>'') FROM audit_run_targets WHERE run_id=$1`, cfg.RunID).Scan(&pending, &retained); err != nil || pending != 0 || retained != 0 {
		t.Fatalf("pending=%d retained=%d err=%v", pending, retained, err)
	}
	page, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 100, Filter: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 12 {
		t.Fatalf("results=%d", len(page.Rows))
	}
	for _, r := range page.Rows {
		u := fmt.Sprint(r["safe_url"])
		if u == server.URL+"/" && (fmt.Sprint(r["broken_internal_links"]) != "1" || fmt.Sprint(r["redirecting_internal_links"]) != "1") {
			t.Fatalf("root metrics=%v", r)
		}
		if u == server.URL+"/d" && fmt.Sprint(r["crawl_depth"]) != "4" {
			t.Fatalf("depth=%v", r)
		}
		if u == server.URL+"/orphan" && (r["orphan_candidate"] != true || r["crawl_depth"] != nil) {
			t.Fatalf("orphan=%v", r)
		}
	}
	var signed int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=$1 AND safe_url LIKE '%/signed?%'`, cfg.RunID).Scan(&signed); err != nil || signed != 2 {
		t.Fatalf("signed=%d err=%v", signed, err)
	}
	var safeJSON []byte
	if err = pool.QueryRow(ctx, `SELECT json_agg(e)::TEXT FROM audit_site_edges e WHERE run_id=$1`, cfg.RunID).Scan(&safeJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(safeJSON), "token=alpha") || strings.Contains(string(safeJSON), "token=beta") {
		t.Fatal("raw query leaked into edges")
	}
	// Interrupted graph batches must not publish provisional metrics as final.
	if _, err = pool.Exec(ctx, `UPDATE audit_site_crawls SET graph_ready=FALSE WHERE run_id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	unfinished, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 100, Filter: "all"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range unfinished.Rows {
		for _, key := range []string{"crawl_depth", "internal_inlinks_count", "internal_outlinks_count", "broken_internal_links", "redirecting_internal_links", "orphan_candidate"} {
			if row[key] != nil {
				t.Fatalf("unfinished graph exposed %s=%v", key, row[key])
			}
		}
	}
	analytics, err := loadWebAnalytics(ctx, pool, cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range analytics.Groups {
		if group.Label == "Граф сайту" {
			for _, bucket := range group.Buckets[:4] {
				if bucket.Value != 0 {
					t.Fatalf("unfinished graph exposed aggregate %s=%v", bucket.Label, bucket.Value)
				}
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/blocked", "/nofollow", "/hidden"} {
		if hits[path] != 0 {
			t.Fatalf("unexpected fetch %s", path)
		}
	}
	if hits["/a"] != 1 || hits["/"] != 1 || hits["/robots.txt"] != 1 {
		t.Fatalf("duplicate fetch: %v", hits)
	}
	index := func(path string) int {
		for i, p := range order {
			if p == path {
				return i
			}
		}
		return -1
	}
	if index("/b") < index("/a") || index("/orphan") < index("/d") {
		t.Fatalf("not BFS: %v", order)
	}
}

func TestSiteDiscoveryFailureRollsBackAndResumes(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<body><a href="/next">Next</a></body>`)
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 5, MaxDepth: 3}); err != nil {
		t.Fatal(err)
	}
	site, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := claimSiteTargetBatch(ctx, pool, cfg, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	target := newAuditTarget(claimed[0], claimed[0].URL, cfg.TargetFingerprintKey)
	if err = markAuditRunTargetStarted(ctx, pool, target, cfg); err != nil {
		t.Fatal(err)
	}
	res := Result{Target: target, Data: SEOData{URL: target.RequestURL, ScanStatus: scanStatusCompleted, StatusCode: httpStatus(200)}}
	results := make(chan Result, 1)
	results <- res
	close(results)
	summary := saveResults(ctx, pool, results, cfg, resultPersistenceHooks{
		before: func(ctx context.Context, tx pgx.Tx) error { return lockSiteOwner(ctx, tx, cfg) },
		after: func(ctx context.Context, tx pgx.Tx, _ Result) error {
			if err := addSiteTargets(ctx, tx, cfg, site, []siteCandidate{{url: server.URL + "/lost", depth: 1}}); err != nil {
				return err
			}
			return fmt.Errorf("injected discovery failure")
		},
	})
	if summary.PersistenceFailures != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	var resultsCount, nodes int
	if err = pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM audit_results WHERE run_id=$1),(SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=$1)`, cfg.RunID).Scan(&resultsCount, &nodes); err != nil || resultsCount != 0 || nodes != 1 {
		t.Fatalf("partial commit: results=%d nodes=%d err=%v", resultsCount, nodes, err)
	}
	if err = completeAuditRun(ctx, pool, cfg.RunID, auditRunCompletion{Status: auditRunStatusFailed, TotalURLs: 1}, cfg); err != nil {
		t.Fatal(err)
	}
	if err = createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("resume exit=%d", code)
	}
}

func TestSiteLimitCancellationAndGraphAPI(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	srv := webFixture()
	defer srv.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: srv.URL, MaxPages: 1, MaxDepth: 0}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	r := httptest.NewRequest("GET", "http://127.0.0.1/api/audits/"+cfg.RunID+"/graph", nil)
	r.Header.Set("Authorization", "Bearer "+testWebToken)
	w := httptest.NewRecorder()
	app.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("graph=%d %s", w.Code, w.Body.String())
	}
	var response struct {
		Site  siteGraphInfo   `json:"site"`
		Edges []siteGraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Site.Nodes != 1 || !response.Site.Limited || !response.Site.Ready || len(response.Edges) != 1 {
		t.Fatalf("graph=%+v", response)
	}
	if strings.Contains(w.Body.String(), "private-value") {
		t.Fatal("graph leaked query")
	}
	// A canceled context must not wait forever for a busy frontier.
	canceled, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	<-canceled.Done()
	if _, err := claimSiteTargetBatch(canceled, pool, cfg, 1); err == nil {
		t.Fatal("frontier ignored cancellation")
	}
}
