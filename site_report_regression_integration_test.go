//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSitemapDoesNotStarvePaginationAndReportExplainsCoverage(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<urlset>")
			for _, path := range []string{"/", "/project/special", "/promo/item"} {
				fmt.Fprintf(w, "<url><loc>http://%s%s</loc></url>", r.Host, path)
			}
			for i := 0; i < 97; i++ {
				fmt.Fprintf(w, "<url><loc>http://%s/seed/%03d</loc></url>", r.Host, i)
			}
			io.WriteString(w, "</urlset>")
			return
		case "/old":
			http.Redirect(w, r, "/new", 301)
			return
		case "/denied":
			w.WriteHeader(403)
			io.WriteString(w, "<h1>Denied</h1>")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/404" {
			io.WriteString(w, `<title>404</title><body><p>Сторінку не знайдено!</p>`)
			return
		}
		io.WriteString(w, `<title>Page</title><body><h1>облік<br>та управління</h1>`)
		switch r.URL.Path {
		case "/":
			io.WriteString(w, `<a href="/projects">Projects</a><a href="/promos">Promos</a><a href="/old">Old</a><a href="/denied">Denied</a><a href="/404">404</a><a href="/unchecked" rel="nofollow">Unchecked</a>`)
		case "/projects":
			io.WriteString(w, strings.Repeat(`<a href="/project/common#image"><img src="x"></a><a href="/project/common">Title</a><a href="/project/common">More</a>`, 150))
			io.WriteString(w, `<a href="/project/special">Special</a>`)
		case "/promos":
			io.WriteString(w, `<a href="/promos?start=40">Next page</a>`)
			if r.URL.RawQuery != "" {
				io.WriteString(w, `<a href="/promo/item">Item</a>`)
			}
		}
		io.WriteString(w, `<p>`+strings.Repeat("Common article content for duplicate comparison. ", 30)+`</p></body>`)
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 16, MaxDepth: 5, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("exit=%d", code)
	}
	page, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 100, Filter: "all"})
	if err != nil {
		t.Fatal(err)
	}
	byURL := map[string]reportRecord{}
	for _, r := range page.Rows {
		byURL[fmt.Sprint(r["safe_url"])] = r
	}
	root := byURL[server.URL+"/"]
	if fmt.Sprint(root["redirecting_internal_links"]) != "1" || fmt.Sprint(root["unchecked_internal_links"]) != "1" || root["crawl_depth_provisional"] != true {
		t.Fatalf("root=%v", root)
	}
	project := byURL[server.URL+"/project/special"]
	if fmt.Sprint(project["crawl_depth"]) != "2" || project["in_sitemap"] != true {
		t.Fatalf("special=%v", project)
	}
	item := byURL[server.URL+"/promo/item"]
	if fmt.Sprint(item["internal_inlinks_count"]) != "1" || item["orphan_candidate"] != false || fmt.Sprint(item["crawl_depth"]) != "3" {
		t.Fatalf("pagination item=%v", item)
	}
	denied := byURL[server.URL+"/denied"]
	if denied["html_parsed"] != false || denied["has_json_ld"] != nil || denied["h1_count"] != nil || denied["is_self_canonical"] != nil {
		t.Fatalf("unparsed=%v", denied)
	}
	if byURL[server.URL+"/404"]["soft_404_status"] != "possible" {
		t.Fatal("soft 404 missed")
	}
	if root["h1"] != "облік та управління" {
		t.Fatal("H1 separator lost", root["h1"])
	}
	coverage, err := loadReportCoverage(ctx, pool, cfg.RunID)
	if err != nil || coverage == nil || coverage.SitemapKnown != 100 || coverage.SitemapChecked >= 100 || !coverage.Partial {
		t.Fatalf("coverage=%+v err=%v", coverage, err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	response := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/export/html", "")
	if response.Code != 200 {
		t.Fatalf("export=%d %s", response.Code, response.Body.String())
	}
	for _, want := range []string{"Повнота обходу", "з 100", "Дублі та схожий контент", "Статус hreflang", "Не перевірено: HTML не розібрано", "попередня оцінка", "Можливий soft 404"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("export missing %q", want)
		}
	}
	other := cfg
	other.RunID = newWebRunID()
	if err = createSiteAuditRun(ctx, pool, &other, siteCrawlOptions{RootURL: server.URL + "/unchecked", MaxPages: 1, MaxDepth: 1}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, other, false); code != exitSuccess {
		t.Fatalf("other run exit=%d", code)
	}
	unchecked, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 100, Filter: "unchecked_internal"})
	if err != nil {
		t.Fatal(err)
	}
	foundRoot := false
	for _, row := range unchecked.Rows {
		if row["safe_url"] == server.URL+"/" {
			foundRoot = true
			if fmt.Sprint(row["unchecked_internal_links"]) != "1" {
				t.Fatal("another run changed the unchecked target count", row)
			}
		}
	}
	if !foundRoot {
		t.Fatal("another run incorrectly satisfied an unchecked target")
	}
	if _, err = pool.Exec(ctx, "UPDATE audit_site_crawls SET graph_ready=false WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	unchecked, err = loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 100, Filter: "unchecked_internal"})
	if err != nil || len(unchecked.Rows) != 0 {
		t.Fatalf("unchecked filter must wait for the graph: %d rows, err=%v", len(unchecked.Rows), err)
	}
}

func TestImageAnalyticsExcludeIncompleteHTML(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	cfg.MaxHTMLBodyBytes = 8 << 10
	cfg.MaxHTMLTokenBytes = 4 << 10
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<body><img src="/image.png">`)
		if r.URL.Path == "/incomplete" {
			io.WriteString(w, strings.Repeat("<p>Normal text</p>", 700))
			return
		}
		io.WriteString(w, `<img src="/second.png" alt="Second image"></body>`)
	}))
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/complete", fixture.URL + "/incomplete"}); err != nil {
		t.Fatal(err)
	}
	executeCapturedAuditRun(ctx, pool, cfg, false)
	page, err := loadResultPage(ctx, pool, cfg.RunID, resultQuery{After: math.MinInt64, Limit: 10, Filter: "errors"})
	if err != nil || len(page.Rows) != 1 || page.Rows[0]["error_code"] != errorCodeResponseParseFailed || page.Rows[0]["html_parsed"] != false || page.Rows[0]["images_missing_alt"] != nil {
		t.Fatalf("expected one failed parse with unknown image metrics: %v, err=%v", page, err)
	}
	analytics, err := loadWebAnalytics(ctx, pool, cfg.RunID)
	if err != nil || analytics.Total != 2 || analytics.Parsed != 1 {
		t.Fatalf("unexpected analytics: total=%d parsed=%d err=%v", analytics.Total, analytics.Parsed, err)
	}
	want := map[string]float64{"Сторінки без alt": 1, "Без alt": 1, "Усього": 2}
	for _, group := range analytics.Groups {
		if group.Label != "Зображення" {
			continue
		}
		for _, bucket := range group.Buckets {
			if expected, ok := want[bucket.Label]; !ok || bucket.Value != expected {
				t.Errorf("%s=%v, want %v", bucket.Label, bucket.Value, expected)
			}
			delete(want, bucket.Label)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing image metrics: %v", want)
	}
}

func TestAutomaticCrawlKeepsResourceHintsOutsidePageBudget(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var assetRequests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, "<urlset><url><loc>http://%s/declared.jpg</loc></url></urlset>", r.Host)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/image-") {
			assetRequests.Add(1)
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "image")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<title>HTML document</title><body><h1>HTML document</h1>")
		if r.URL.Path == "/" {
			for i := 0; i < 80; i++ {
				fmt.Fprintf(w, `<a href="/image-%d.png?token=private">Image</a>`, i)
			}
			io.WriteString(w, `<a href="/page">Page</a><a href="/page?start=40">Pagination</a><a href="/declared.jpg">Declared HTML</a><a href="/manual.png">Manual override</a>`)
		}
		io.WriteString(w, "</body>")
	}))
	defer fixture.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 4, MaxDepth: 3, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	if assetRequests.Load() != 0 {
		t.Fatal("resource URLs used HTML request budget", assetRequests.Load())
	}
	c, err := loadReportCoverage(ctx, pool, cfg.RunID)
	if err != nil || c == nil || c.ResourceHints != 81 || c.Checked != 4 || c.SitemapChecked != 1 {
		t.Fatalf("coverage=%+v err=%v", c, err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/coverage?filter=resources", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"resource_hint"`) || strings.Contains(w.Body.String(), "token=private") {
		t.Fatalf("resources: %d %s", w.Code, w.Body.String())
	}
	cfg.RunID = newWebRunID()
	if err = createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/manual.png"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	var title string
	if err = pool.QueryRow(ctx, "SELECT title FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&title); err != nil || title != "HTML document" {
		t.Fatal("explicit resource-like HTML was filtered", title, err)
	}
}

func TestDeferredSitemapPromotionCanBeRebuiltAndCancelled(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	cfg.Workers = 1
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		if r.URL.Path == "/sitemap.xml" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, "<urlset><url><loc>http://%s/</loc></url><url><loc>http://%s/a</loc></url><url><loc>http://%s/b</loc></url></urlset>", r.Host, r.Host, r.Host)
			return
		}
		http.NotFound(w, r)
	}))
	defer fixture.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 10, MaxDepth: 3, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	site, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := fixture.Client()
	cache := newRobotsPolicyCache(time.Minute, 2)
	if err = initializeSiteSitemaps(ctx, pool, cfg, site, client, client, cache); err != nil {
		t.Fatal(err)
	}
	var total int
	if err = pool.QueryRow(ctx, "SELECT total_urls FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&total); err != nil || total != 1 || len(site.sitemapCandidates) != 3 {
		t.Fatalf("prefilled: total=%d candidates=%d err=%v", total, len(site.sitemapCandidates), err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = promoteSiteSitemapBatch(cancelled, pool, cfg, site); err == nil || len(site.sitemapCandidates) != 3 {
		t.Fatal("cancelled promotion consumed candidates", err)
	}
	if err = promoteSiteSitemapBatch(ctx, pool, cfg, site); err != nil {
		t.Fatal(err)
	}
	var firstID int64
	if err = pool.QueryRow(ctx, "SELECT target_id FROM audit_site_nodes WHERE run_id=$1 AND safe_url=$2", cfg.RunID, fixture.URL+"/a").Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = initializeSiteSitemaps(ctx, pool, cfg, rebuilt, client, client, cache); err != nil {
		t.Fatal(err)
	}
	for len(rebuilt.sitemapCandidates) > 0 {
		if err = promoteSiteSitemapBatch(ctx, pool, cfg, rebuilt); err != nil {
			t.Fatal(err)
		}
	}
	var repeatedID int64
	if err = pool.QueryRow(ctx, "SELECT target_id FROM audit_site_nodes WHERE run_id=$1 AND safe_url=$2", cfg.RunID, fixture.URL+"/a").Scan(&repeatedID); err != nil || repeatedID != firstID {
		t.Fatal("snapshot identity changed", err)
	}
	if err = pool.QueryRow(ctx, "SELECT total_urls FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&total); err != nil || total != 3 {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestInaccessibleHTMLKeepsGraphConfidenceProvisional(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/denied" {
			w.WriteHeader(403)
			io.WriteString(w, "Denied")
			return
		}
		io.WriteString(w, `<body><a href="/denied">Denied</a></body>`)
	}))
	defer fixture.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: fixture.URL, MaxPages: 10, MaxDepth: 3}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	c, err := loadReportCoverage(ctx, pool, cfg.RunID)
	if err != nil || c == nil || c.Known != 2 || c.Checked != 2 || !c.GraphPartial {
		t.Fatalf("coverage=%+v err=%v", c, err)
	}
}
