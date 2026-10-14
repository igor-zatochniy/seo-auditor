package seo

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func comparisonPage(t *testing.T, body string) Data {
	t.Helper()
	r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "X-Robots-Tag": []string{"googlebot: noarchive"}}, Body: io.NopCloser(strings.NewReader(body))}
	d, err := ParsePageForComparison(r, "https://example.com/page", 8<<20, 5<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRenderComparisonAllMetrics(t *testing.T) {
	raw := comparisonPage(t, `<html><head><title>Before</title><meta name="description" content="old"><link rel="canonical" href="/old"></head><body>one two <a href="/old-link">old</a></body></html>`)
	dom := comparisonPage(t, `<html><head><title>After</title><meta name="description" content="new"><link rel="canonical" href="/new"><meta name="robots" content="noindex"><link rel="alternate" hreflang="uk" href="/uk"><script type="application/ld+json">{"@type":"Article"}</script></head><body><h1>New heading</h1>one two three four <a href="/new-link">new</a></body></html>`)
	a, b := Snapshot(raw), Snapshot(dom)
	d := Compare(a, b)
	if !d.Title || !d.Description || !d.Canonical || !d.Robots || !d.H1 || !d.Text || !d.Links || !d.JSONLD || !d.Hreflang || !d.H1OnlyAfterRendering || !d.JSONLDInjected {
		t.Fatalf("missing delta: %+v", d)
	}
	if d.AddedInternalLinks != 1 || d.RemovedInternalLinks != 1 || d.TextGrowthPercent == nil {
		t.Fatalf("incorrect counts: %+v", d)
	}
	if a.XRobotsTag != b.XRobotsTag || !a.Complete || !b.Complete {
		t.Fatal("headers or completeness changed")
	}
	unchanged := Compare(a, Snapshot(raw))
	if unchanged.Title || unchanged.Text || unchanged.Links || unchanged.JSONLD || unchanged.Hreflang {
		t.Fatalf("false delta: %+v", unchanged)
	}
}

func TestComparisonPreservesOriginalSEOMetrics(t *testing.T) {
	body := `<html><head><title>Stable</title><meta name="description" content="same"><script type="application/ld+json">{"x":1}</script></head><body><h1>Heading</h1>some words <a href="/a">A</a><img><template><h1>Inert</h1></template></body></html>`
	response := func() *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
	}
	a, err := ParsePageWithLinks(response(), "https://example.com/page", 8<<20, 5<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParsePageForComparison(response(), "https://example.com/page", 8<<20, 5<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	b.comparison = nil
	if !reflect.DeepEqual(a, b) {
		t.Fatal("comparison modified baseline SEO metrics")
	}
}

func TestComparisonEvidenceLimitsAndInertTemplates(t *testing.T) {
	body := `<head>` + strings.Repeat(`<link rel="alternate" hreflang="uk" href="/uk">`, 40) + `</head><body>real<template><h1>inert</h1><a href="/hidden">hidden</a><script type="application/ld+json">{}</script></template></body>`
	s := Snapshot(comparisonPage(t, body))
	if s.Complete || len(s.Hreflang) != 32 || s.H1Count != 0 || s.JSONLDCount != 0 || len(s.evidence.links) != 0 {
		t.Fatalf("unbounded or inert evidence: %+v", s)
	}
	if Compare(s, s).TextGrowthPercent != nil {
		t.Fatal("partial evidence produced exact percentage")
	}
}

func TestComparisonMarksTruncatedDescriptionIncomplete(t *testing.T) {
	body := `<head><meta name="description" content="` + strings.Repeat("a", StorageDescriptionMaxRunes+1) + `"></head><body>Page content</body>`
	s := Snapshot(comparisonPage(t, body))
	if s.Complete || Compare(s, s).TextGrowthPercent != nil {
		t.Fatal("truncated description reported as complete evidence")
	}
}

func TestMainContentDependencyDetectsJSOnlyContentAndHydration(t *testing.T) {
	raw := Snapshot(comparisonPage(t, `<html><body><main><div id="app"></div></main></body></html>`))
	dom := Snapshot(comparisonPage(t, `<html><body><main>`+strings.Repeat("rendered ", 180)+`</main></body></html>`))
	diagnostic := AssessMainContentDependency(raw, dom, true)
	if diagnostic.Status != "high" || diagnostic.RawWords != 0 || diagnostic.RenderedWords < mainContentSubstantialWords {
		t.Fatalf("client-rendered content not detected: %+v", diagnostic)
	}

	content := strings.Repeat("shared article content ", 100)
	server := Snapshot(comparisonPage(t, `<html><body><nav>`+strings.Repeat("navigation ", 120)+`</nav><main>`+content+`</main></body></html>`))
	hydrated := Snapshot(comparisonPage(t, `<html><body><nav>`+strings.Repeat("different menu ", 120)+`</nav><main>`+content+`<button>Client control</button></main></body></html>`))
	diagnostic = AssessMainContentDependency(server, hydrated, true)
	if diagnostic.Status != "not_detected" || diagnostic.SimilarityPercent == nil || *diagnostic.SimilarityPercent != 100 {
		t.Fatalf("hydration with unchanged main content was flagged: %+v", diagnostic)
	}
}

func TestMainContentDependencyDetectsReplacementAtSimilarWordCount(t *testing.T) {
	server := Snapshot(comparisonPage(t, `<html><body><main>`+strings.Repeat("alpha topic source ", 80)+`</main></body></html>`))
	dom := Snapshot(comparisonPage(t, `<html><body><main>`+strings.Repeat("omega product answer ", 80)+`</main></body></html>`))
	diagnostic := AssessMainContentDependency(server, dom, true)
	if server.MainContentWords != dom.MainContentWords || diagnostic.Status != "warning" || diagnostic.SimilarityPercent == nil || *diagnostic.SimilarityPercent >= mainContentSimilarityFloor {
		t.Fatalf("same-size replacement was missed: raw=%d rendered=%d result=%+v", server.MainContentWords, dom.MainContentWords, diagnostic)
	}
}

func TestMainContentDependencyIgnoresNavigationAndMarksIncompleteUnknown(t *testing.T) {
	navigationOnly := `<html><body><nav>` + strings.Repeat("menu navigation item ", 100) + `</nav></body></html>`
	raw := Snapshot(comparisonPage(t, navigationOnly))
	dom := Snapshot(comparisonPage(t, navigationOnly))
	diagnostic := AssessMainContentDependency(raw, dom, true)
	if raw.MainContentWords != 0 || diagnostic.Status != "unknown" {
		t.Fatalf("navigation text was treated as main content: raw=%d result=%+v", raw.MainContentWords, diagnostic)
	}
	diagnostic = AssessMainContentDependency(raw, dom, false)
	if diagnostic.Status != "unknown" {
		t.Fatalf("incomplete rendering was not unknown: %+v", diagnostic)
	}
}
