package main

import (
	"context"
	"strings"
	"testing"
)

func TestSiteURLScopeAndIdentity(t *testing.T) {
	for _, pair := range [][2]string{{"https://BÜCHER.de:443", "https://xn--bcher-kva.de/"}, {"https://example.com/a#part", "https://example.com/a"}} {
		u, _, err := normalizeSiteURL(pair[0], false)
		if err != nil || u != pair[1] {
			t.Fatalf("url=%q err=%v", u, err)
		}
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://user:pass@example.com/", "file:///tmp/test", "https://example.com/" + strings.Repeat("x", 2048)} {
		if _, _, err := normalizeSiteURL(raw, false); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	a, _, _ := normalizeSiteURL("https://example.com/?token=alpha", false)
	b, _, _ := normalizeSiteURL("https://example.com/?token=beta", false)
	if a == b || redactURL(a) != redactURL(b) {
		t.Fatal("identity and display URL are not separated")
	}
}

func TestSitemapParserAndBudgets(t *testing.T) {
	cases := []struct {
		name, xml            string
		limit, count         int
		index, limited, fail bool
	}{
		{"urls", `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://example.com/?a=1&amp;b=2</loc><image:image xmlns:image="urn:images"><image:loc>ignore</image:loc></image:image></url></urlset>`, 10, 1, false, false, false},
		{"index", `<sitemapindex><sitemap><loc>https://example.com/a.xml.gz</loc></sitemap></sitemapindex>`, 10, 1, true, false, false},
		{"cutoff", `<urlset><url><loc>a</loc></url><url><loc>b</loc></url></urlset>`, 1, 1, false, true, false},
		{"entity", `<!DOCTYPE x [<!ENTITY secret SYSTEM "file:///etc/passwd">]><urlset><url><loc>&secret;</loc></url></urlset>`, 10, 0, false, false, true},
		{"nesting", `<urlset>` + strings.Repeat("<x>", 33), 10, 0, false, false, true},
		{"large", `<urlset>` + strings.Repeat(" ", maxSitemapBytes), 10, 0, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			urls, index, limited, err := parseSitemap(context.Background(), strings.NewReader(tc.xml), tc.limit)
			if (err != nil) != tc.fail || len(urls) != tc.count || index != tc.index || limited != tc.limited {
				t.Fatalf("urls=%v index=%v limited=%v err=%v", urls, index, limited, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := parseSitemap(ctx, strings.NewReader(`<urlset/>`), 10); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestSiteGraphClickDepthsWithCyclesAndRedirects(t *testing.T) {
	adj := map[int64][]graphArc{1: {{2, 1}, {4, 0}}, 2: {{3, 1}}, 3: {{1, 1}}, 4: {{3, 1}}, 9: {{10, 1}}}
	d, err := siteClickDepths(context.Background(), adj)
	if err != nil || d[1] != 0 || d[2] != 1 || d[3] != 1 || d[4] != 0 || len(d) != 4 {
		t.Fatalf("depths=%v err=%v", d, err)
	}
}

func TestRobotsSitemapsBoundedAndLineEndings(t *testing.T) {
	locations := robotsSitemapLocations("\ufeffSitemap: https://example.com/1.xml\rSITEMAP: https://example.com/2.xml\nUser-agent: *\n" + strings.Repeat("Sitemap: https://example.com/3.xml\n", 30))
	if len(locations) != maxSitemapFiles || locations[0] != "https://example.com/1.xml" || locations[1] != "https://example.com/2.xml" {
		t.Fatalf("locations=%v", locations)
	}
}
