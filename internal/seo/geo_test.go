package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func parseGEOTest(t *testing.T, body string) Data {
	t.Helper()
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
	data, err := ParsePage(response, "https://example.com/page", 8<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGEOMainContentSelection(t *testing.T) {
	for _, tc := range []struct {
		name, body, source, excerpt, heading string
		author                               bool
	}{
		{"main-after-menu", `<nav><p>` + strings.Repeat("menu ", 3000) + `</p><ul><li>item</li></ul></nav><main><h1>Main heading</h1><p>Real answer</p></main><footer>footer</footer>`, "main", "Real answer", "Main heading", false},
		{"article-header", `<header>site header</header><article><header><h1>Article heading</h1><a rel="author">Editor</a><time>2026-10-05</time></header><p>Real answer</p></article>`, "article", "2026-10-05", "Article heading", true},
		{"role-main", `<div role="navigation">menu</div><div role="main"><h2>Main heading</h2><p>Real answer</p></div>`, "main", "Real answer", "Main heading", false},
		{"body-fallback", `<header>site header</header><nav>menu</nav><h1>Main heading</h1><p>Real answer</p><aside>aside</aside><footer>footer</footer>`, "body", "Real answer", "Main heading", false},
		{"hidden", `<main><section hidden><p>menu</p><table></table></section><div aria-hidden="true">footer</div><h2>Main heading</h2><p>Real answer</p></main>`, "main", "Real answer", "Main heading", false},
		{"nested-article", `<main><article><header><h1>Main heading</h1></header><p>Real answer</p></article><aside>aside</aside></main>`, "main", "Real answer", "Main heading", false},
		{"prefer-main", `<article><h1>Teaser</h1><p>teaser</p></article><main><h1>Main heading</h1><p>Real answer</p></main>`, "main", "Real answer", "Main heading", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := parseGEOTest(t, "<body>"+tc.body+"</body>")
			s := data.GEO
			if s.Version != 3 || s.ContentSource != tc.source || !strings.Contains(s.Excerpt, tc.excerpt) || s.Headings != tc.heading || s.HasAuthor != tc.author || s.HasTable || s.HasList || s.FirstParagraph != "Real answer" {
				t.Fatalf("Хибна вибірка: %+v", s)
			}
			for _, excluded := range []string{"menu", "footer", "aside", "teaser"} {
				if strings.Contains(s.Excerpt, excluded) {
					t.Fatalf("Службовий контент: %q", s.Excerpt)
				}
			}
		})
	}
}

func TestGEOCleanMainContentDoesNotChangeDocumentMetrics(t *testing.T) {
	body := `<body><main><header><p>menu</p><ul><li>menu</li></ul></header><form><p>menu</p><table></table></form><div role="form"><p>menu</p></div><nav>menu</nav><aside>menu</aside><dialog>menu</dialog><article><header><h1>Answer</h1><a rel="author">Editor</a></header><p>Real answer<p>Second paragraph<footer><p>menu</p><ul><li>menu</li></ul></footer></article></main></body>`
	data := parseGEOTest(t, body)
	s := data.GEO
	if s.ContentSource != "main" || strings.Contains(s.Excerpt, "menu") || s.HasList || s.HasTable || !s.HasAuthor || s.Headings != "Answer" || s.FirstParagraph != "Real answer" {
		t.Fatalf("Службовий контент вплинув на GEO: %+v", s)
	}
	if data.H1 != "Answer" || data.H1Count != 1 || data.WordCount == 0 {
		t.Fatal("Звичайні SEO-метрики змінено")
	}
	for _, empty := range []string{`<main></main>`, `<main><nav>menu</nav></main>`, `<main hidden>menu</main>`} {
		data = parseGEOTest(t, `<body>`+empty+`<article><p>Real answer</p></article></body>`)
		if data.GEO.ContentSource != "article" || data.GEO.FirstParagraph != "Real answer" {
			t.Fatalf("Порожній main заблокував fallback: %+v", data.GEO)
		}
	}
}

func TestGEOStructureLimitDoesNotBreakSEO(t *testing.T) {
	data := parseGEOTest(t, `<body><h1>Title</h1>`+strings.Repeat("<div>", 200)+`<p>Real answer words</p><img src="x"><a href="/page">Link</a>`+strings.Repeat("</div>", 200))
	if !data.GEO.ContentIncomplete || data.H1 != "Title" || data.H1Count != 1 || data.TotalImages != 1 || data.ImagesMissingAlt != 1 || data.InternalLinksCount != 1 || data.WordCount != 3 {
		t.Fatalf("GEO пошкодив SEO: %+v", data)
	}
}

func TestGEOSnippetControlsRespectScopeAndRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, meta     string
		headers        []string
		index, snippet string
		max            *int
	}{
		{"other-agent", `<meta name="googlebot-news" content="noindex,nosnippet">`, []string{"GPTBot: noindex,nosnippet"}, geo.Allowed, geo.Allowed, nil},
		{"global", `<meta name="robots" content="noindex,nosnippet"><meta name="googlebot" content="index">`, nil, geo.Blocked, geo.Blocked, nil},
		{"header-scoped", ``, []string{"googlebot: noindex, max-snippet: 0", "bingbot: max-snippet: -1"}, geo.Blocked, geo.Blocked, intPtr(0)},
		{"restrictive-limit", `<meta name="googlebot" content="max-snippet: 150">`, []string{"max-snippet: 50"}, geo.Allowed, geo.Allowed, intPtr(50)},
		{"inert", `<template><meta name="robots" content="noindex"></template>`, nil, geo.Allowed, geo.Allowed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: 200, Header: http.Header{"X-Robots-Tag": tc.headers}, Body: io.NopCloser(strings.NewReader(`<head>` + tc.meta + `</head><body><div data-nosnippet>Secret excerpt</div><p>Answer</p></body>`))}
			data, err := ParsePage(response, "https://example.com", 8<<20, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			s := data.GEO.Search
			if s.GoogleIndexing != tc.index || s.GoogleSnippet != tc.snippet || !s.DataNoSnippet || s.GooglebotRules != geo.Unknown || s.OAISearchBotRules != geo.Unknown || (s.MaxSnippet == nil) != (tc.max == nil) {
				t.Fatalf("Неправильні правила: %+v", s)
			}
			if tc.max != nil && *tc.max != *s.MaxSnippet {
				t.Fatal("Загублено суворіше обмеження")
			}
		})
	}
}

func intPtr(value int) *int { return &value }

func TestGEOSignalsAreBoundedAndIgnoreInertContent(t *testing.T) {
	data := parseGEOTest(t, `<head><title>Stripe protection</title><meta name="author" content="Editor"><script type="application/ld+json">{"@graph":[{"@type":"Organization"},{"@type":["Product","Thing"]}]}</script></head><body><h1>Protection</h1><p>First answer about Stripe.</p><template><h2>Hidden heading</h2><table></table><p>Hidden answer</p><script type="application/ld+json">{"@type":"FAQPage"}</script></template><ul><li>Evidence</li></ul>`+strings.Repeat(" довгий текст", 1000)+`</body>`)
	s := data.GEO
	if s == nil || !s.Complete || !s.SampleTruncated || !s.HasAuthor || !s.HasList || s.HasTable {
		t.Fatalf("Некоректні сигнали: %+v", s)
	}
	if s.FirstParagraph != "First answer about Stripe." || strings.Contains(s.Excerpt, "Hidden") || strings.Contains(s.Headings, "Hidden") {
		t.Fatal("Витік інертного вмісту")
	}
	if strings.Join(s.SchemaTypes, ",") != "Organization,Product" {
		t.Fatalf("Типи: %v", s.SchemaTypes)
	}
	if len([]rune(s.Excerpt)) > 2000 || len([]rune(s.Headings)) > 800 {
		t.Fatal("Перевищено ліміт тексту")
	}
}

func TestGEOInvalidSchemaAndFailedHTMLRemainUnknown(t *testing.T) {
	data := parseGEOTest(t, `<script type="application/ld+json">{invalid}</script><body><p>Answer</p></body>`)
	if !data.GEO.SchemaIncomplete || len(data.GEO.SchemaTypes) != 0 {
		t.Fatal("Некоректний JSON-LD прийнято")
	}
	data = parseGEOTest(t, `<script type="application/ld+json">`+strings.Repeat(" ", 70000)+`</script><p>Answer</p>`)
	if !data.GEO.SchemaIncomplete {
		t.Fatal("Ліміт JSON-LD не застосовано")
	}
	response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))}
	data, err := ParsePage(response, "https://example.com", 20, 20)
	if err == nil || data.GEO != nil {
		t.Fatal("Частковий HTML видається за повний")
	}
}
