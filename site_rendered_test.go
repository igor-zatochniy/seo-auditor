package main

import (
	"fmt"
	"github.com/igor-zatochniy/seo-auditor/internal/seo"
	"testing"
)

func TestRenderedDiscoveryPreservesRawMetricsAndDeduplicatesGraph(t *testing.T) {
	a := seo.DiscoveredLink{URL: "https://example.com/a", Anchor: "A"}
	d := SEOData{WordCount: 7, DiscoveredLinks: []seo.DiscoveredLink{a, a}, Rendering: &seo.RenderComparison{Status: "completed", Rendered: &seo.RenderSnapshot{Complete: true}, DiscoveredLinks: []seo.DiscoveredLink{a, a, {URL: "https://example.com/js", Nofollow: true}}}}
	links, limited := siteDiscoveryLinks(d, true, false)
	if limited || len(links) != 2 || links[0].Source != "both" || links[1].Source != "rendered" || !links[1].Nofollow {
		t.Fatalf("links=%+v limited=%v", links, limited)
	}
	if d.WordCount != 7 || len(d.DiscoveredLinks) != 2 || d.DiscoveredLinks[0].Source != "" {
		t.Fatal("raw data mutated")
	}
	links, _ = siteDiscoveryLinks(d, false, false)
	if len(links) != 1 || links[0].Source != "html" {
		t.Fatal("HTML-only discovery changed")
	}
}

func TestRenderedDiscoveryIsBoundedAndExplicitlyPartial(t *testing.T) {
	d := SEOData{DiscoveredLinks: make([]seo.DiscoveredLink, seo.MaxDiscoveredLinks), Rendering: &seo.RenderComparison{Status: "partial", Rendered: &seo.RenderSnapshot{Complete: true}, DiscoveredLinks: []seo.DiscoveredLink{{URL: "https://example.com/js"}}}}
	for i := range d.DiscoveredLinks {
		d.DiscoveredLinks[i].URL = fmt.Sprintf("https://example.com/%d", i)
	}
	links, limited := siteDiscoveryLinks(d, true, false)
	if len(links) != seo.MaxDiscoveredLinks || !limited {
		t.Fatal("combined discovery is not bounded")
	}
	d.Rendering = nil
	if _, limited = siteDiscoveryLinks(d, true, false); !limited {
		t.Fatal("unavailable rendering marked complete")
	}
}
