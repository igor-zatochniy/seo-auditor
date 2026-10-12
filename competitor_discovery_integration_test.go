//go:build integration

package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func storeDiscoveryFixture(t *testing.T, pool *pgxpool.Pool, cfg Config, queries []string) {
	t.Helper()
	manifest := competitorDiscovery{Version: geoDiscoveryVersion, QueryHash: "fixture", Queries: queries}
	if _, err := pool.Exec(t.Context(), "UPDATE audit_site_crawls SET competitor_selection=$2 WHERE run_id=$1", cfg.RunID, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestCompetitorDiscoveryFindsDeepSitemapPageWithoutUnsafeRequests(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var outsideHits, noiseHits, blockedHits, secretHits atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outsideHits.Add(1) }))
	defer external.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprintf(w, "User-agent: *\nDisallow: /blocked/\nDisallow: /denied.xml\nSitemap: %s/index.xml\nSitemap: %s/denied.xml\n", base, base)
		case "/index.xml":
			fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/pages.xml.gz</loc></sitemap><sitemap><loc>%s/escape.xml</loc></sitemap></sitemapindex>`, base, base)
		case "/escape.xml":
			http.Redirect(w, r, external.URL+"/sitemap.xml", http.StatusFound)
		case "/pages.xml.gz":
			z := gzip.NewWriter(w)
			io.WriteString(z, "<urlset>")
			for i := range 55 {
				fmt.Fprintf(z, "<url><loc>%s/news/item-%d</loc></url>", base, i)
			}
			for _, raw := range []string{base + "/solutions/payments/business/stripe-chargeback-protection", base + "/blocked/stripe-chargeback-protection", base + "/stripe-chargeback-protection?token=private-secret", external.URL + "/stripe-chargeback-protection"} {
				fmt.Fprintf(z, "<url><loc>%s</loc></url>", raw)
			}
			io.WriteString(z, "</urlset>")
			z.Close()
		case "/denied.xml", "/blocked/stripe-chargeback-protection":
			blockedHits.Add(1)
		case "/sitemap.xml":
			http.NotFound(w, r)
		default:
			if strings.HasPrefix(r.URL.Path, "/news/") {
				noiseHits.Add(1)
			}
			if r.URL.RawQuery != "" {
				secretHits.Add(1)
			}
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><head><title>Stripe chargeback protection</title></head><body><h1>Stripe chargeback protection</h1><p>Stripe chargeback protection software.</p><a href="/news/item-0">News</a></body></html>`)
		}
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 3, MaxDepth: 2, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	storeDiscoveryFixture(t, pool, cfg, []string{"best Stripe chargeback protection"})
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	if outsideHits.Load()+noiseHits.Load()+blockedHits.Load()+secretHits.Load() != 0 {
		t.Fatal("Небезпечні або нерелевантні HTTP-запити", outsideHits.Load(), noiseHits.Load(), blockedHits.Load(), secretHits.Load())
	}
	var total, deep int
	var manifest competitorDiscovery
	if err := pool.QueryRow(ctx, `SELECT total_urls,(SELECT COUNT(*) FROM audit_results a WHERE a.run_id=r.id AND a.safe_url LIKE '%/solutions/payments/business/stripe-chargeback-protection' AND a.status_code=200),sc.competitor_selection
	 FROM audit_runs r JOIN audit_site_crawls sc ON sc.run_id=r.id WHERE r.id=$1`, cfg.RunID).Scan(&total, &deep, &manifest); err != nil {
		t.Fatal(err)
	}
	if total != 3 || deep != 1 || manifest.Summary == nil || manifest.Summary.CandidateURLs != 57 || manifest.Summary.SelectedURLs != 3 || manifest.Summary.MatchingQueries != 1 || manifest.Summary.State != "partial" || !manifest.Summary.SelectionLimited {
		t.Fatal(total, deep, manifest)
	}
	var retained int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1 AND request_url<>''", cfg.RunID).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("Термінальний запуск залишив request_url", retained, err)
	}
}

func TestCompetitorDiscoveryCancellationDoesNotPublishPartialSelection(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<urlset><url><loc>http://%s/solutions/chargeback-protection</loc></url></urlset>`, r.Host)
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 3, MaxDepth: 2, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	storeDiscoveryFixture(t, pool, cfg, []string{"chargeback protection"})
	site, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err = initializeSiteSitemaps(canceled, pool, cfg, site, client, client, newRobotsPolicyCache(time.Minute, 2)); err == nil {
		t.Fatal("Скасоване discovery завершилося успішно")
	}
	var count int
	var state string
	if err = pool.QueryRow(ctx, "SELECT total_urls,sitemap_state FROM audit_runs r JOIN audit_site_crawls s ON s.run_id=r.id WHERE r.id=$1", cfg.RunID).Scan(&count, &state); err != nil || count != 1 || state != "pending" {
		t.Fatal(count, state, err)
	}
	if err = initializeSiteSitemaps(ctx, pool, cfg, site, client, client, newRobotsPolicyCache(time.Minute, 2)); err != nil {
		t.Fatal(err)
	}
	if err = initializeSiteSitemaps(ctx, pool, cfg, site, client, client, newRobotsPolicyCache(time.Minute, 2)); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 2 {
		t.Fatal("Повторне discovery продублювало targets", count, err)
	}
}

func TestCompetitorDiscoveryReportsSitemapCutoff(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "<urlset>")
		for i := range 5000 {
			fmt.Fprintf(w, "<url><loc>http://%s/news/item-%d</loc></url>", r.Host, i)
		}
		fmt.Fprintf(w, "<url><loc>http://%s/solutions/chargeback-protection</loc></url></urlset>", r.Host)
	}))
	defer server.Close()
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: server.URL, MaxPages: 3, MaxDepth: 2, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	storeDiscoveryFixture(t, pool, cfg, []string{"chargeback protection"})
	site, err := loadSiteCrawl(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := initializeSiteSitemaps(ctx, pool, cfg, site, client, client, newRobotsPolicyCache(time.Minute, 2)); err != nil {
		t.Fatal(err)
	}
	var summary competitorDiscoverySummary
	var total int
	if err := pool.QueryRow(ctx, `SELECT sc.competitor_selection->'summary',r.total_urls FROM audit_site_crawls sc JOIN audit_runs r ON r.id=sc.run_id WHERE r.id=$1`, cfg.RunID).Scan(&summary, &total); err != nil {
		t.Fatal(err)
	}
	if !summary.DiscoveryLimited || summary.State != "partial" || summary.CandidateURLs != 5000 || summary.SelectedURLs != 1 || total != 1 {
		t.Fatal("Усічення sitemap приховано або бюджет перевищено", summary, total)
	}
}

func TestCompetitorDiscoveryReusesOnlyMatchingQuerySnapshots(t *testing.T) {
	app, parent, cfg := visibilityReportFixture(t)
	cleanupStrategy(t, app)
	ctx := t.Context()
	app.manager = newAuditManager(ctx, app.pool, cfg)
	app.manager.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	t.Cleanup(app.manager.shutdown)
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := prepareCompetitorDiscovery(ctx, tx, cfg, parent.ID)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://first.example/", "https://second.example/"} {
		v := cfg
		v.RunID = newWebRunID()
		if err := createSiteAuditRun(ctx, app.pool, &v, siteCrawlOptions{RootURL: raw, MaxPages: 5, MaxDepth: 2, UseSitemaps: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.pool.Exec(ctx, "UPDATE audit_site_crawls SET competitor_selection=$2,sitemap_state='done' WHERE run_id=$1", v.RunID, selection); err != nil {
			t.Fatal(err)
		}
		if err := finishStrategyFixture(ctx, app.pool, v, false); err != nil {
			t.Fatal(err)
		}
	}
	var scans atomic.Int32
	app.manager.execute = func(c context.Context, p *pgxpool.Pool, v Config, _ bool) int {
		scans.Add(1)
		if err := finishStrategyFixture(c, p, v, false); err != nil {
			t.Error(err)
			return exitFatal
		}
		return exitSuccess
	}
	newSet := func() string {
		input := competitorSetInput{ID: newWebRunID(), MaxPages: 5, Competitors: []competitorInput{{URL: "first.example"}, {URL: "second.example"}}}
		if err := app.storeCompetitorSet(ctx, parent.ID, input); err != nil {
			t.Fatal(err)
		}
		return input.ID
	}
	first := newSet()
	for range 2 {
		if _, err := app.manager.startCompetitor(ctx); err != nil {
			t.Fatal(err)
		}
		if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	var reused int
	if err := app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND reused", first).Scan(&reused); err != nil || reused != 2 || scans.Load() != 0 {
		t.Fatal(reused, scans.Load(), err)
	}
	w := strategyRequest(app, "GET", "/api/geo/competitors/"+first, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), selection.QueryHash) {
		t.Fatal("Приватний manifest потрапив до API", w.Code)
	}
	if _, err := app.pool.Exec(ctx, `UPDATE audit_site_crawls SET competitor_selection=jsonb_set(competitor_selection,'{version}','"previous-discovery-model"') WHERE origin='https://first.example'`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, "UPDATE audit_site_crawls SET sitemap_state='partial' WHERE origin='https://second.example'"); err != nil {
		t.Fatal(err)
	}
	oldModel := newSet()
	for range 2 {
		if _, err := app.manager.startCompetitor(ctx); err != nil {
			t.Fatal(err)
		}
		waitScheduled(t, app.manager)
		if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err := app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND reused", oldModel).Scan(&reused); err != nil || reused != 0 || scans.Load() != 2 {
		t.Fatal("Стара модель або partial discovery використані повторно", reused, scans.Load(), err)
	}
	if _, err := app.pool.Exec(ctx, `UPDATE geo_query_results SET result=jsonb_set(result,'{query}','"Stripe-specific alternatives"') WHERE report_id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	second := newSet()
	for range 2 {
		if _, err := app.manager.startCompetitor(ctx); err != nil {
			t.Fatal(err)
		}
		waitScheduled(t, app.manager)
		if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err := app.pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND reused", second).Scan(&reused); err != nil || reused != 0 || scans.Load() != 4 {
		t.Fatal("Інші запити використали старий відбір", reused, scans.Load(), err)
	}
}

func TestCompetitorDiscoveryUpgradeFrom25PreservesReports(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	before := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/results", "")
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(t.Context(), db, migrationDir, 25); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrations(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	after := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/results", "")
	if before.Code != 200 || after.Code != 200 || before.Body.String() != after.Body.String() {
		t.Fatal("SEO-історія змінилася після міграції", before.Code, after.Code)
	}
	var count int
	if err = app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_query_results WHERE report_id=$1", report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("GEO-історія змінилася", count, err)
	}
}
