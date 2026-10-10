//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
)

func TestInternationalSiteCrawlComparesHTMLAndSitemap(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var origin string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/sitemap.xml" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:x="http://www.w3.org/1999/xhtml"><url><loc>%s/en</loc><x:link rel="alternate" hreflang="en" href="%s/en"/><x:link rel="alternate" hreflang="es" href="%s/es"/></url><url><loc>%s/es</loc><x:link rel="alternate" hreflang="en" href="%s/en"/><x:link rel="alternate" hreflang="es" href="%s/es"/></url></urlset>`, origin, origin, origin, origin, origin, origin)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<head><title>Page</title><link rel="canonical" href="%s%s"><link rel="alternate" hreflang="%s" href="%s%s"></head><body><h1>Page</h1>Content</body>`, origin, r.URL.Path, r.URL.Path[1:], origin, r.URL.Path)
	}))
	defer fixture.Close()
	origin = fixture.URL
	if err := createSiteAuditRun(ctx, pool, &cfg, siteCrawlOptions{RootURL: origin + "/en", MaxPages: 3, MaxDepth: 2, UseSitemaps: true}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/international", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var data struct {
		Pages []international.Page `json:"pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Pages) != 2 {
		t.Fatal(data.Pages)
	}
	for _, p := range data.Pages {
		if p.Sitemap == nil || len(p.Sitemap.Links) != 2 {
			t.Fatal("sitemap evidence missing", p)
		}
		found := false
		for _, i := range p.Issues {
			found = found || i.Code == "html_sitemap_mismatch"
		}
		if !found {
			t.Fatal("different HTML/sitemap sets accepted", p)
		}
	}
}
