package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParsePageExtractsSEOMetrics(t *testing.T) {
	title := "Technical SEO Audit Platform for Modern Websites"
	description := strings.TrimSpace(strings.Repeat("Detailed content quality signal. ", 5))
	html := `<!doctype html>
<html>
<head>
  <title>` + title + `</title>
  <meta name="description" content="` + description + `">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="index,follow">
  <meta property="og:title" content="Open Graph Title">
  <meta property="og:description" content="Open Graph Description">
  <meta property="og:image" content="https://example.com/og.png">
  <meta name="twitter:card" content="summary_large_image">
  <link rel="canonical" href="/page">
  <script type="application/ld+json">{"@context":"https://schema.org"}</script>
</head>
<body>
  <h1>Primary heading</h1>
  <h2>Section</h2>
  <h3>Subsection</h3>
  <a href="/internal">Internal</a>
  <a href="https://example.com/absolute">Same host</a>
  <a href="https://external.test/page">External</a>
  <a href="mailto:team@example.com">Mail</a>
  <img src="/missing-alt.png">
  <img src="/empty-alt.png" alt=" ">
  <img src="/chart.png" alt="Chart">
  <p>Useful body copy for word count.</p>
</body>
</html>`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"X-Robots-Tag": []string{"index, follow"},
		},
		Body: io.NopCloser(strings.NewReader(html)),
	}

	data, err := parsePage(resp, "https://example.com/page", DefaultMaxHTMLBodyBytes, DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parsePage returned error: %v", err)
	}

	if data.Title != title || data.TitleStatus != "OK" {
		t.Fatalf("unexpected title result: %q / %q", data.Title, data.TitleStatus)
	}
	if data.Description != description || data.DescriptionStatus != "OK" {
		t.Fatalf("unexpected description result: %q / %q", data.Description, data.DescriptionStatus)
	}
	if data.H1 != "Primary heading" || data.H1Count != 1 {
		t.Fatalf("unexpected H1 result: %q / %d", data.H1, data.H1Count)
	}
	if data.H2ToH6Status != "H2:1, H3:1" {
		t.Fatalf("unexpected heading structure: %q", data.H2ToH6Status)
	}
	if data.InternalLinksCount != 2 || data.ExternalLinksCount != 1 || data.LinksCount != 3 {
		t.Fatalf("unexpected links: internal=%d external=%d total=%d", data.InternalLinksCount, data.ExternalLinksCount, data.LinksCount)
	}
	if !data.IsSelfCanonical || data.CanonicalURL != "/page" {
		t.Fatalf("unexpected canonical result: %q / %t", data.CanonicalURL, data.IsSelfCanonical)
	}
	if !data.HasJsonLd || !data.HasViewport {
		t.Fatalf("expected JSON-LD and viewport flags")
	}
	if data.TotalImages != 3 || data.ImagesMissingAlt != 1 {
		t.Fatalf("unexpected image audit: total=%d missing=%d", data.TotalImages, data.ImagesMissingAlt)
	}
	if data.MetaRobots != "index, follow" || data.XRobotsTag != "index, follow" {
		t.Fatalf("unexpected robots metadata: meta=%q header=%q", data.MetaRobots, data.XRobotsTag)
	}
	if data.WordCount == 0 {
		t.Fatalf("expected non-zero word count")
	}
}

func TestParsePageCountsOnlyImagesWithoutAltAttribute(t *testing.T) {
	html := `<html><body>
  <img src="missing.png">
  <img src="boolean.png" alt>
  <img src="empty.png" alt="">
  <img src="whitespace.png" alt=" ">
  <img src="described.png" alt="Chart">
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(html)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(html)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.TotalImages != 5 || data.ImagesMissingAlt != 1 {
		t.Fatalf(
			"image counts = total %d missing_alt %d, want 5 and 1",
			data.TotalImages,
			data.ImagesMissingAlt,
		)
	}
}

func TestParsePageAggregatesAllRobotsDirectives(t *testing.T) {
	tests := []struct {
		name     string
		meta     string
		expected string
	}{
		{
			name: "index before noindex",
			meta: `<meta name="robots" content="index,follow">
<meta name="robots" content="noindex,nofollow">`,
			expected: "noindex, nofollow, index, follow",
		},
		{
			name: "noindex before index",
			meta: `<meta name="robots" content="noindex,nofollow">
<meta name="robots" content="index,follow">`,
			expected: "noindex, nofollow, index, follow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "<html><head>" + tt.meta + "</head><body>Content</body></html>"
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/html; charset=utf-8"},
					"X-Robots-Tag": []string{"index, follow", "noindex, nofollow"},
				},
				Body: io.NopCloser(strings.NewReader(body)),
			}

			data, err := parsePage(resp, "https://example.com", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
			if err != nil {
				t.Fatalf("parse page: %v", err)
			}
			if data.MetaRobots != tt.expected {
				t.Fatalf("meta robots = %q, want %q", data.MetaRobots, tt.expected)
			}
			if data.XRobotsTag != "noindex, nofollow, index, follow" {
				t.Fatalf("X-Robots-Tag = %q", data.XRobotsTag)
			}
		})
	}
}

func TestRobotsHeaderDirectivesKeepsNoindexWithinStorageBound(t *testing.T) {
	header := http.Header{
		"X-Robots-Tag": []string{
			strings.Repeat("unknown", storageRobotsTagMaxRunes),
			"googlebot: noindex",
		},
	}

	value, truncated, originalLength := robotsHeaderDirectives(header)
	if !truncated || originalLength <= storageRobotsTagMaxRunes {
		t.Fatalf("robots metadata was not truncated: truncated=%t original=%d", truncated, originalLength)
	}
	if !strings.Contains(strings.ToLower(value), "noindex") {
		t.Fatalf("bounded robots metadata lost noindex: %q", value)
	}
}

func TestRobotsHeaderDirectivesPreservesScopeAndCommaSensitiveRules(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected string
	}{
		{
			name:     "googlebot scoped rules",
			header:   "googlebot: nofollow, noindex",
			expected: "googlebot: noindex, googlebot: nofollow",
		},
		{
			name:     "arbitrary crawler scope",
			header:   "otherbot: noindex, nofollow",
			expected: "otherbot: noindex, otherbot: nofollow",
		},
		{
			name:     "parameterized generic rule",
			header:   "max-snippet:20, noindex",
			expected: "noindex, max-snippet:20",
		},
		{
			name:     "generic unavailable after with comma",
			header:   "unavailable_after: Fri, 25 Jun 2010 15:00:00 PST",
			expected: "unavailable_after: Fri, 25 Jun 2010 15:00:00 PST",
		},
		{
			name:   "scoped unavailable after followed by rule",
			header: "googlebot: unavailable_after: Fri, 25 Jun 2010 15:00:00 PST, noindex",
			expected: "googlebot: noindex, " +
				"googlebot: unavailable_after: Fri, 25 Jun 2010 15:00:00 PST",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, truncated, _ := robotsHeaderDirectives(http.Header{"X-Robots-Tag": []string{tt.header}})
			if truncated {
				t.Fatal("robots header was unexpectedly truncated")
			}
			if value != tt.expected {
				t.Fatalf("X-Robots-Tag = %q, want %q", value, tt.expected)
			}
		})
	}
}

func TestParsePagePreservesCommaInScopedMetaUnavailableAfter(t *testing.T) {
	body := `<html><head>
<meta name="googlebot" content="unavailable_after: Fri, 25 Jun 2010 15:00:00 PST">
</head><body>Content</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	want := "googlebot: unavailable_after: Fri, 25 Jun 2010 15:00:00 PST"
	if data.MetaRobots != want {
		t.Fatalf("meta robots = %q, want %q", data.MetaRobots, want)
	}
}

func TestStorageSanitizerTruncatesOversizedHTMLMetadata(t *testing.T) {
	longTitle := strings.Repeat("T", storageTitleMaxRunes+25)
	longDescription := strings.Repeat("D", storageDescriptionMaxRunes+25)
	longOGDescription := strings.Repeat("G", storageDescriptionMaxRunes+25)
	longH1 := strings.Repeat("H", storageH1MaxRunes+25)
	longOGTitle := strings.Repeat("O", storageTitleMaxRunes+25)
	longTwitterCard := strings.Repeat("C", storageTwitterCardMaxRunes+25)
	longCanonical := "https://example.com/" + strings.Repeat("canonical", 260)
	longRobots := strings.Repeat("R", storageRobotsTagMaxRunes+25)
	html := `<!doctype html>
<html>
<head>
  <title>` + longTitle + `</title>
	<meta name="description" content="` + longDescription + `">
  <link rel="canonical" href="` + longCanonical + `">
  <meta name="robots" content="` + longRobots + `">
  <meta property="og:title" content="` + longOGTitle + `">
	<meta property="og:description" content="` + longOGDescription + `">
  <meta property="og:image" content="https://cdn.example.com/` + strings.Repeat("image", 420) + `.png?token=secret">
  <meta name="twitter:card" content="` + longTwitterCard + `">
</head>
<body><h1>` + longH1 + `</h1></body>
</html>`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"X-Robots-Tag": []string{longRobots},
		},
		Body: io.NopCloser(strings.NewReader(html)),
	}

	data, err := parsePage(resp, "https://example.com/page", DefaultMaxHTMLBodyBytes, DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parsePage returned error: %v", err)
	}
	if data.TitleStatus != "Too Long" ||
		utf8.RuneCountInString(data.Title) != storageTitleMaxRunes ||
		!data.TitleTruncated ||
		data.TitleOriginalLength != storageTitleMaxRunes+25 {
		t.Fatalf(
			"parser did not bound oversized title with telemetry: status=%q length=%d truncated=%t original=%d",
			data.TitleStatus,
			utf8.RuneCountInString(data.Title),
			data.TitleTruncated,
			data.TitleOriginalLength,
		)
	}

	stored := sanitizeSEODataForStorage(data)
	if got := utf8.RuneCountInString(data.Description); got != storageDescriptionMaxRunes {
		t.Fatalf("description parser length = %d, want %d", got, storageDescriptionMaxRunes)
	}
	if got := utf8.RuneCountInString(data.OGDescription); got != storageDescriptionMaxRunes {
		t.Fatalf("og_description parser length = %d, want %d", got, storageDescriptionMaxRunes)
	}
	if got := utf8.RuneCountInString(stored.Description); got != storageDescriptionMaxRunes {
		t.Fatalf("description stored length = %d, want %d", got, storageDescriptionMaxRunes)
	}
	if got := utf8.RuneCountInString(stored.OGDescription); got != storageDescriptionMaxRunes {
		t.Fatalf("og_description stored length = %d, want %d", got, storageDescriptionMaxRunes)
	}
	assertTruncatedField(t, "title", stored.Title, stored.TitleTruncated, stored.TitleOriginalLength, storageTitleMaxRunes, storageTitleMaxRunes+25)
	assertTruncatedField(t, "h1", stored.H1, stored.H1Truncated, stored.H1OriginalLength, storageH1MaxRunes, storageH1MaxRunes+25)
	assertTruncatedField(t, "og_title", stored.OGTitle, stored.OGTitleTruncated, stored.OGTitleOriginalLength, storageTitleMaxRunes, storageTitleMaxRunes+25)
	assertTruncatedField(t, "twitter_card", stored.TwitterCard, stored.TwitterCardTruncated, stored.TwitterCardOriginalLength, storageTwitterCardMaxRunes, storageTwitterCardMaxRunes+25)
	assertTruncatedAtLimit(t, "canonical_url", stored.CanonicalURL, stored.CanonicalURLTruncated, stored.CanonicalURLOriginalLength, storageURLMaxRunes)
	assertTruncatedAtLimit(t, "og_image", stored.OGImage, stored.OGImageTruncated, stored.OGImageOriginalLength, storageURLMaxRunes)
	assertTruncatedAtLimit(t, "meta_robots", stored.MetaRobots, stored.MetaRobotsTruncated, stored.MetaRobotsOriginalLength, storageRobotsTagMaxRunes)
	assertTruncatedAtLimit(t, "x_robots_tag", stored.XRobotsTag, stored.XRobotsTagTruncated, stored.XRobotsTagOriginalLength, storageRobotsTagMaxRunes)
}

func assertTruncatedField(t *testing.T, name, value string, truncated bool, originalLength, maxRunes, expectedOriginalLength int) {
	t.Helper()
	if !truncated {
		t.Fatalf("%s was not marked as truncated", name)
	}
	if originalLength != expectedOriginalLength {
		t.Fatalf("%s original length = %d, want %d", name, originalLength, expectedOriginalLength)
	}
	if got := utf8.RuneCountInString(value); got != maxRunes {
		t.Fatalf("%s stored length = %d, want %d", name, got, maxRunes)
	}
}

func assertTruncatedAtLimit(t *testing.T, name, value string, truncated bool, originalLength, maxRunes int) {
	t.Helper()
	if !truncated || originalLength <= maxRunes {
		t.Fatalf("%s was not marked with a valid original length: truncated=%t original=%d", name, truncated, originalLength)
	}
	if got := utf8.RuneCountInString(value); got != maxRunes {
		t.Fatalf("%s stored length = %d, want %d", name, got, maxRunes)
	}
}

func TestParsePageRejectsOversizedHTML(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("abcdef")),
	}

	_, err := parsePage(resp, "https://example.com", 5, DefaultMaxHTMLTokenBytes)
	if err == nil {
		t.Fatalf("expected body size limit error")
	}
}

func TestParsePageRejectsOversizedHTMLToken(t *testing.T) {
	body := "<html><body><script>" + strings.Repeat("x", 1024) + "</script></body></html>"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	_, err := parsePage(resp, "https://example.com", int64(len(body)+1), 256)
	if err == nil || !strings.Contains(err.Error(), "HTML token exceeds configured limit") {
		t.Fatalf("expected oversized token error, got %v", err)
	}
}

func TestParsePageStreamsTagDenseHTML(t *testing.T) {
	const tagCount = 100_000
	body := "<html><body>" + strings.Repeat("<i>x</i>", tagCount) + "</body></html>"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(
		resp,
		"https://example.com",
		int64(len(body)+1),
		DefaultMaxHTMLTokenBytes,
	)
	if err != nil {
		t.Fatalf("stream tag-dense HTML: %v", err)
	}
	if data.WordCount != 1 {
		t.Fatalf("unexpected streamed word count: got %d want 1", data.WordCount)
	}
}

func TestParsePageExcludesScriptAndStyleFromWordCount(t *testing.T) {
	body := `<html><body>
<p>one two three</p>
<script type="application/ld+json">fake script words must not count</script>
<style>.message { content: "fake style words"; }</style>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.WordCount != 3 {
		t.Fatalf("word count = %d, want 3", data.WordCount)
	}
	if !data.HasJsonLd {
		t.Fatal("JSON-LD marker was not detected")
	}
}

func TestParsePageCountsBodyWordsWhenHeadEndTagIsOmitted(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "explicit body",
			body: `<html><head><title>Example</title><body><p>one two three</p></body></html>`,
		},
		{
			name: "implicit body",
			body: `<html><head><title>Example</title><p>one two three</p></html>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}

			data, err := parsePage(resp, "https://example.com", int64(len(tt.body)+1), DefaultMaxHTMLTokenBytes)
			if err != nil {
				t.Fatalf("parse page: %v", err)
			}
			if data.WordCount != 3 {
				t.Fatalf("word count = %d, want 3", data.WordCount)
			}
		})
	}
}

func TestParsePageCountsDirectTextAfterImplicitHead(t *testing.T) {
	body := `<html><head><title>Page</title>one two three</html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.WordCount != 3 {
		t.Fatalf("word count = %d, want 3", data.WordCount)
	}
}

func TestParsePageAcceptsHeadOnlyMetadataBeforeBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "explicit head",
			body: `<html><head><link rel="canonical" href="/page">` +
				`<meta name="robots" content="index,follow"></head><body>Content</body></html>`,
		},
		{
			name: "implicit head",
			body: `<html><link rel="canonical" href="/page">` +
				`<meta name="robots" content="index,follow"><body>Content</body></html>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}

			data, err := parsePage(resp, "https://example.com/page", int64(len(tt.body)+1), DefaultMaxHTMLTokenBytes)
			if err != nil {
				t.Fatalf("parse page: %v", err)
			}
			if data.CanonicalURL != "/page" || !data.IsSelfCanonical {
				t.Fatalf("canonical = %q self=%t, want /page and true", data.CanonicalURL, data.IsSelfCanonical)
			}
			if data.MetaRobots != "index, follow" {
				t.Fatalf("meta robots = %q, want %q", data.MetaRobots, "index, follow")
			}
		})
	}
}

func TestParsePageReadsBodyRobotsAndIgnoresOtherBodyMetadata(t *testing.T) {
	body := `<html><head></head><body>
<title>Body title</title>
<link rel="canonical" href="/page">
<meta name="description" content="Body description">
<meta name="robots" content="noindex">
<meta name="viewport" content="width=device-width">
<meta property="og:title" content="Body Open Graph title">
Visible content
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.Title != "" || data.CanonicalURL != "" || data.IsSelfCanonical {
		t.Fatalf(
			"body metadata leaked into title/canonical: title=%q canonical=%q self=%t",
			data.Title,
			data.CanonicalURL,
			data.IsSelfCanonical,
		)
	}
	if data.Description != "" || data.HasViewport || data.OGTitle != "" {
		t.Fatalf(
			"head-only body metadata leaked into SEO fields: description=%q viewport=%t og_title=%q",
			data.Description,
			data.HasViewport,
			data.OGTitle,
		)
	}
	if data.MetaRobots != "noindex" {
		t.Fatalf("body meta robots = %q, want noindex", data.MetaRobots)
	}
	if data.WordCount != 2 {
		t.Fatalf("word count = %d, want 2", data.WordCount)
	}
}

func TestParsePagePreservesScopedGoogleRobotsMetadata(t *testing.T) {
	body := `<html><head>
<meta name="robots" content="index,follow">
<meta name="googlebot" content="noindex,nofollow">
</head><body>
<meta name="GOOGLEBOT-NEWS" content="nosnippet">
Content
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	want := "googlebot: noindex, googlebot: nofollow, index, follow, googlebot-news: nosnippet"
	if data.MetaRobots != want {
		t.Fatalf("meta robots = %q, want %q", data.MetaRobots, want)
	}
}

func TestParsePageIgnoresTemplateContentAcrossSEOMetrics(t *testing.T) {
	body := `<html><head>
<template>
  <title>Template title</title>
  <link rel="canonical" href="https://other.example/page">
  <meta name="description" content="Template description">
  <meta name="robots" content="noindex">
  <meta name="viewport" content="width=device-width">
  <script type="application/ld+json">{"name":"Template data"}</script>
  <h1>Fake heading</h1>
  <a href="https://external.example/">Fake link</a>
  <img src="fake.jpg">
  hidden template words
  <template shadowrootmode="open">
    <h1>Nested shadow heading</h1>
    nested shadow words
  </template>
</template>
</head><body>
  <h1>Real heading</h1>
  <a href="/about">Internal link</a>
  <img src="real.jpg" alt="Real image">
  <p>real visible words</p>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.Title != "" || data.Description != "" || data.CanonicalURL != "" || data.MetaRobots != "" {
		t.Fatalf(
			"template metadata leaked into SEO fields: title=%q description=%q canonical=%q robots=%q",
			data.Title,
			data.Description,
			data.CanonicalURL,
			data.MetaRobots,
		)
	}
	if data.HasViewport || data.HasJsonLd {
		t.Fatalf("template feature flags leaked: viewport=%t json_ld=%t", data.HasViewport, data.HasJsonLd)
	}
	if data.H1 != "Real heading" || data.H1Count != 1 {
		t.Fatalf("H1 = %q count=%d, want real heading and 1", data.H1, data.H1Count)
	}
	if data.InternalLinksCount != 1 || data.ExternalLinksCount != 0 || data.LinksCount != 1 {
		t.Fatalf(
			"link counts = internal %d external %d total %d, want 1, 0, 1",
			data.InternalLinksCount,
			data.ExternalLinksCount,
			data.LinksCount,
		)
	}
	if data.TotalImages != 1 || data.ImagesMissingAlt != 0 {
		t.Fatalf(
			"image counts = total %d missing_alt %d, want 1 and 0",
			data.TotalImages,
			data.ImagesMissingAlt,
		)
	}
	if data.WordCount != 7 {
		t.Fatalf("word count = %d, want 7", data.WordCount)
	}
}

func TestParsePageIncludesDeclarativeShadowDOMContent(t *testing.T) {
	for _, mode := range []string{"open", "closed"} {
		t.Run(mode, func(t *testing.T) {
			body := `<html><head>
<title>Docs</title>
<meta name="robots" content="index,follow">
</head><body><docs-page>
<template shadowrootmode="` + mode + `">
  <title>Shadow title</title>
  <link rel="canonical" href="https://other.example/page">
  <meta name="description" content="Shadow description">
  <meta name="robots" content="noindex">
  <meta name="googlebot" content="nofollow">
  <script type="application/ld+json">{"name":"Shadow data"}</script>
  <h1>Documentation</h1>
  <p>Important visible documentation text</p>
  <a href="/install">Install</a>
  <img src="/hero.jpg" alt="">
  <template><h1>Inert nested heading</h1>hidden nested words</template>
</template>
</docs-page></body></html>`
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}

			data, err := parsePage(resp, "https://example.com/docs", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
			if err != nil {
				t.Fatalf("parse page: %v", err)
			}
			if data.Title != "Docs" || data.Description != "" || data.CanonicalURL != "" {
				t.Fatalf(
					"shadow metadata leaked into document fields: title=%q description=%q canonical=%q",
					data.Title,
					data.Description,
					data.CanonicalURL,
				)
			}
			if data.MetaRobots != "index, follow" || data.HasJsonLd {
				t.Fatalf("shadow directives leaked: robots=%q json_ld=%t", data.MetaRobots, data.HasJsonLd)
			}
			if data.H1 != "Documentation" || data.H1Count != 1 {
				t.Fatalf("H1 = %q count=%d, want Documentation and 1", data.H1, data.H1Count)
			}
			if data.InternalLinksCount != 1 || data.ExternalLinksCount != 0 || data.LinksCount != 1 {
				t.Fatalf(
					"link counts = internal %d external %d total %d, want 1, 0, 1",
					data.InternalLinksCount,
					data.ExternalLinksCount,
					data.LinksCount,
				)
			}
			if data.TotalImages != 1 || data.ImagesMissingAlt != 0 {
				t.Fatalf(
					"image counts = total %d missing_alt %d, want 1 and 0",
					data.TotalImages,
					data.ImagesMissingAlt,
				)
			}
			if data.WordCount != 6 {
				t.Fatalf("word count = %d, want 6", data.WordCount)
			}
		})
	}
}

func TestParsePageTreatsInvalidShadowRootModeAsInert(t *testing.T) {
	body := `<html><body>
<template shadowrootmode="invalid"><h1>Hidden heading</h1>hidden words</template>
<p>Visible content</p>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.H1Count != 0 || data.WordCount != 2 {
		t.Fatalf("invalid shadow root leaked content: h1_count=%d word_count=%d", data.H1Count, data.WordCount)
	}
}

func TestParsePageUsesBaseHrefForCanonicalAndRelativeLinks(t *testing.T) {
	body := `<html><head>
<link rel="canonical" href="docs/page">
<base href="https://example.com/">
</head><body>
<a href="relative">Relative link</a>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/docs/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if !data.IsSelfCanonical {
		t.Fatal("relative canonical was not resolved against document base URL")
	}
	if data.InternalLinksCount != 1 || data.ExternalLinksCount != 0 {
		t.Fatalf("link counts = internal %d external %d, want 1 and 0", data.InternalLinksCount, data.ExternalLinksCount)
	}
}

func TestParsePageRecognizesCanonicalRelToken(t *testing.T) {
	body := `<html><head><link rel="alternate canonical" href="/page"></head><body>Content</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.CanonicalURL != "/page" || !data.IsSelfCanonical {
		t.Fatalf("canonical = %q self=%t", data.CanonicalURL, data.IsSelfCanonical)
	}
}

func TestParsePageUsesFirstCrossHostBaseAndSkipsNonWebSchemes(t *testing.T) {
	body := `<html><head>
<base href="https://assets.example.net/root/">
<base href="https://example.com/">
</head><body>
<a href="relative">Cross-host relative</a>
<a href="//example.com/network-path">Same-host network path</a>
<a href="data:text/plain,hello">Data</a>
<a href="sms:+380000000000">SMS</a>
<a href="custom-protocol:value">Custom</a>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if data.InternalLinksCount != 1 || data.ExternalLinksCount != 1 || data.LinksCount != 2 {
		t.Fatalf(
			"link counts = internal %d external %d total %d, want 1, 1, 2",
			data.InternalLinksCount,
			data.ExternalLinksCount,
			data.LinksCount,
		)
	}
}

func TestParsePageNormalizesDefaultPortsForCanonicalAndLinks(t *testing.T) {
	body := `<html><head>
<link rel="canonical" href="https://example.com:443/page">
</head><body>
<a href="https://example.com:443/about">Default HTTPS port</a>
<a href="https://example.com:8443/admin">Non-default port</a>
</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, "https://example.com/page", int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if !data.IsSelfCanonical {
		t.Fatal("canonical with the explicit default HTTPS port was not self-canonical")
	}
	if data.InternalLinksCount != 1 || data.ExternalLinksCount != 1 {
		t.Fatalf(
			"link counts = internal %d external %d, want 1 and 1",
			data.InternalLinksCount,
			data.ExternalLinksCount,
		)
	}
}

func TestParsePageNormalizesIDNForCanonicalAndLinks(t *testing.T) {
	tests := []struct {
		name      string
		targetURL string
		aliasHost string
	}{
		{name: "target punycode", targetURL: "https://xn--bcher-kva.de/page", aliasHost: "bücher.de"},
		{name: "target unicode", targetURL: "https://bücher.de/page", aliasHost: "xn--bcher-kva.de"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `<html><head><link rel="canonical" href="https://` + tt.aliasHost + `/page"></head><body>` +
				`<a href="https://` + tt.aliasHost + `/about">About</a></body></html>`
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}

			data, err := parsePage(resp, tt.targetURL, int64(len(body)+1), DefaultMaxHTMLTokenBytes)
			if err != nil {
				t.Fatalf("parse page: %v", err)
			}
			if !data.IsSelfCanonical {
				t.Fatal("IDN alias canonical was not classified as self-canonical")
			}
			if data.InternalLinksCount != 1 || data.ExternalLinksCount != 0 {
				t.Fatalf(
					"link counts = internal %d external %d, want 1 and 0",
					data.InternalLinksCount,
					data.ExternalLinksCount,
				)
			}
		})
	}
}

func TestParsePageComparesSelfCanonicalBeforeStorageTruncation(t *testing.T) {
	prefix := "https://example.com/"
	targetURL := prefix + strings.Repeat("a", storageURLMaxRunes-len(prefix))
	canonicalURL := targetURL + "-different"
	body := `<html><head><link rel="canonical" href="` + canonicalURL + `"></head><body></body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	data, err := parsePage(resp, targetURL, int64(len(body)+1), DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	if !data.CanonicalURLTruncated {
		t.Fatal("oversized canonical was not marked as truncated")
	}
	if data.CanonicalURL != targetURL {
		t.Fatal("stored canonical does not contain the expected bounded representation")
	}
	if data.IsSelfCanonical {
		t.Fatal("truncated canonical was incorrectly classified as self-canonical")
	}
}

func TestParsePagePreservesNonOKStatus(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Header: http.Header{
			"X-Robots-Tag": []string{"noindex"},
		},
		Body: io.NopCloser(strings.NewReader("not found")),
	}

	data, err := parsePage(resp, "https://example.com/missing", DefaultMaxHTMLBodyBytes, DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parsePage returned error for non-200 status: %v", err)
	}
	if data.StatusCode == nil || *data.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected status code: got %v want %d", data.StatusCode, http.StatusNotFound)
	}
	if data.XRobotsTag != "noindex" {
		t.Fatalf("unexpected X-Robots-Tag: %q", data.XRobotsTag)
	}
}

func BenchmarkParsePageTagDenseHTML(b *testing.B) {
	body := "<html><body>" + strings.Repeat("<i>x</i>", 100_000) + "</body></html>"
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))

	for range b.N {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		if _, err := parsePage(
			resp,
			"https://example.com",
			int64(len(body)+1),
			DefaultMaxHTMLTokenBytes,
		); err != nil {
			b.Fatalf("stream tag-dense HTML: %v", err)
		}
	}
}

func TestParsePageDecodesDeclaredCharset(t *testing.T) {
	body := []byte("<html><head><title>caf\xe9 audit</title></head><body><h1>caf\xe9</h1></body></html>")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/html; charset=iso-8859-1"},
		},
		Body: io.NopCloser(strings.NewReader(string(body))),
	}

	data, err := parsePage(resp, "https://example.com", DefaultMaxHTMLBodyBytes, DefaultMaxHTMLTokenBytes)
	if err != nil {
		t.Fatalf("parsePage returned error: %v", err)
	}
	if data.Title != "caf\u00e9 audit" || data.H1 != "caf\u00e9" {
		t.Fatalf("charset was not decoded: title=%q h1=%q", data.Title, data.H1)
	}
}

func TestValidateHTMLContentTypeUsesParsedMediaType(t *testing.T) {
	valid := []string{
		"text/html; charset=utf-8",
		"application/xhtml+xml",
	}
	for _, contentType := range valid {
		if err := validateHTMLContentType(contentType); err != nil {
			t.Fatalf("expected %q to be accepted: %v", contentType, err)
		}
	}

	invalid := []string{
		"application/json; profile=text/html",
		"text/html garbage",
	}
	for _, contentType := range invalid {
		if err := validateHTMLContentType(contentType); err == nil {
			t.Fatalf("expected %q to be rejected", contentType)
		}
	}
}

func TestIsSelfCanonical(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		target    string
		want      bool
	}{
		{name: "missing canonical is not self", canonical: "", target: "https://example.com/page", want: false},
		{name: "relative canonical", canonical: "/page", target: "https://example.com/page", want: true},
		{name: "non-root trailing slash differs", canonical: "/page/", target: "https://example.com/page", want: false},
		{name: "root slash is equivalent", canonical: "https://example.com/", target: "https://example.com", want: true},
		{name: "default HTTPS port is equivalent", canonical: "https://example.com:443/page", target: "https://example.com/page", want: true},
		{name: "default HTTP port is equivalent", canonical: "http://example.com:80/page", target: "http://example.com/page", want: true},
		{name: "non-default port differs", canonical: "https://example.com:8443/page", target: "https://example.com/page", want: false},
		{name: "different host", canonical: "https://other.test/page", target: "https://example.com/page", want: false},
		{name: "different path", canonical: "/other", target: "https://example.com/page", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSelfCanonical(tt.canonical, tt.target); got != tt.want {
				t.Fatalf("isSelfCanonical() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRobotsRules(t *testing.T) {
	wildcard := `
User-agent: *
Disallow: /private/
Allow: /private/open
`
	if isPathAllowedByRobots(wildcard, UserAgentStr, "/private/page") {
		t.Fatalf("expected wildcard Disallow rule to block /private/page")
	}
	if !isPathAllowedByRobots(wildcard, UserAgentStr, "/private/open") {
		t.Fatalf("expected longer Allow rule to permit /private/open")
	}

	specific := `
User-agent: *
Disallow: /

User-agent: Go-SEOParser-Bot
Allow: /audit$
`
	if !isPathAllowedByRobots(specific, UserAgentStr, "/audit") {
		t.Fatalf("expected specific user-agent group to permit /audit")
	}
	if !isPathAllowedByRobots(specific, UserAgentStr, "/other") {
		t.Fatalf("expected unmatched path in selected group to be allowed")
	}
}

func TestRobotsHTTPClientFollowsFiveRedirects(t *testing.T) {
	redirects := map[string]string{
		"/robots.txt": "/redirect-1",
		"/redirect-1": "/redirect-2",
		"/redirect-2": "/redirect-3",
		"/redirect-3": "/redirect-4",
		"/redirect-4": "/rules",
	}
	var rulesFetched atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location, ok := redirects[r.URL.Path]; ok {
			http.Redirect(w, r, location, http.StatusFound)
			return
		}
		if r.URL.Path == "/rules" {
			rulesFetched.Store(true)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /blocked\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := newRobotsHTTPClient(server.Client().Transport)
	allowed, err := isAllowedByRobots(context.Background(), client, server.URL+"/blocked", time.Second)
	if err != nil {
		t.Fatalf("isAllowedByRobots returned error: %v", err)
	}
	if allowed {
		t.Fatal("expected redirected robots.txt rule to disallow /blocked")
	}
	if !rulesFetched.Load() {
		t.Fatal("robots client did not follow five redirects to the rules")
	}
}

func TestRobotsHTTPClientStopsAfterFiveRedirects(t *testing.T) {
	redirects := map[string]string{
		"/robots.txt": "/redirect-1",
		"/redirect-1": "/redirect-2",
		"/redirect-2": "/redirect-3",
		"/redirect-3": "/redirect-4",
		"/redirect-4": "/redirect-5",
		"/redirect-5": "/rules",
	}
	var rulesFetched atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location, ok := redirects[r.URL.Path]; ok {
			http.Redirect(w, r, location, http.StatusFound)
			return
		}
		if r.URL.Path == "/rules" {
			rulesFetched.Store(true)
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := newRobotsHTTPClient(server.Client().Transport)
	allowed, err := isAllowedByRobots(context.Background(), client, server.URL+"/page", time.Second)
	if err == nil {
		t.Fatal("expected redirect limit error")
	}
	if allowed {
		t.Fatal("unresolved robots.txt redirect chain must fail closed")
	}
	if rulesFetched.Load() {
		t.Fatal("robots client followed more than five redirects")
	}
}

func TestRobotsAccessStatusHandling(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantAllowed bool
		wantErr     bool
	}{
		{name: "404 is unavailable", status: http.StatusNotFound, wantAllowed: true},
		{name: "503 is unreachable", status: http.StatusServiceUnavailable, wantAllowed: false, wantErr: true},
		{name: "unresolved redirect fails closed", status: http.StatusFound, wantAllowed: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			client := newRobotsHTTPClient(server.Client().Transport)
			allowed, err := isAllowedByRobots(context.Background(), client, server.URL+"/page", time.Second)
			if allowed != tt.wantAllowed {
				t.Fatalf("isAllowedByRobots allowed=%t, want %t", allowed, tt.wantAllowed)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("isAllowedByRobots error=%v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestRobotsNetworkErrorFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	client := newRobotsHTTPClient(server.Client().Transport)
	targetURL := server.URL + "/page"
	server.Close()

	allowed, err := isAllowedByRobots(context.Background(), client, targetURL, time.Second)
	if err == nil {
		t.Fatal("expected robots.txt network error")
	}
	if allowed {
		t.Fatal("network error must not allow page scanning")
	}
}

func TestWorkerDoesNotFetchPageWhenRobotsIsUnreachable(t *testing.T) {
	var pageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		pageRequests.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><h1>Must not be fetched</h1></body></html>")
	}))
	defer server.Close()

	targetURL := server.URL + "/page"
	jobs := make(chan AuditTarget, 1)
	jobs <- newAuditTarget(targetURLRecord{ID: 1, URL: targetURL}, targetURL, []byte(testTargetFingerprintKey))
	close(jobs)
	results := make(chan Result, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go worker(
		context.Background(),
		context.Background(),
		1,
		jobs,
		results,
		server.Client(),
		newRobotsHTTPClient(server.Client().Transport),
		newRobotsPolicyCache(time.Minute, 16),
		nil,
		Config{
			HTTPAttemptTimeout:   time.Second,
			HTTPTotalTimeout:     5 * time.Second,
			RobotsAttemptTimeout: time.Second,
			RobotsTotalTimeout:   5 * time.Second,
			MaxHTMLBodyBytes:     DefaultMaxHTMLBodyBytes,
		},
		&wg,
	)
	wg.Wait()
	close(results)

	result, ok := <-results
	if !ok {
		t.Fatal("worker did not report robots.txt failure")
	}
	if result.Error == nil {
		t.Fatal("expected robots.txt 503 to become a task error")
	}
	if result.Target.TargetID != 1 {
		t.Fatalf("unexpected target ID: %d", result.Target.TargetID)
	}
	if result.Data.URL != targetURL {
		t.Fatalf("unexpected result URL: %q", result.Data.URL)
	}
	if result.Data.StatusCode != nil {
		t.Fatalf("page status must be null when robots.txt is unreachable: %v", result.Data.StatusCode)
	}
	if result.Data.ScanStatus != scanStatusFailed || result.Data.ErrorCode != errorCodeRobotsUnavailable {
		t.Fatalf("unexpected scan failure outcome: status=%q code=%q", result.Data.ScanStatus, result.Data.ErrorCode)
	}
	if result.Data.RobotsOutcome != robotsOutcomeUnavailable {
		t.Fatalf("unexpected robots outcome: %q", result.Data.RobotsOutcome)
	}
	if pageRequests.Load() != 0 {
		t.Fatalf("page was fetched despite unreachable robots.txt: requests=%d", pageRequests.Load())
	}
}

func TestWorkerReportsRobotsDisallowWithoutPageStatus(t *testing.T) {
	var pageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /page\n")
			return
		}
		pageRequests.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><h1>Must not be fetched</h1></body></html>")
	}))
	defer server.Close()

	targetURL := server.URL + "/page"
	jobs := make(chan AuditTarget, 1)
	jobs <- newAuditTarget(targetURLRecord{ID: 1, URL: targetURL}, targetURL, []byte(testTargetFingerprintKey))
	close(jobs)
	results := make(chan Result, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go worker(
		context.Background(),
		context.Background(),
		1,
		jobs,
		results,
		server.Client(),
		newRobotsHTTPClient(server.Client().Transport),
		newRobotsPolicyCache(time.Minute, 16),
		nil,
		Config{
			HTTPAttemptTimeout:   time.Second,
			HTTPTotalTimeout:     5 * time.Second,
			RobotsAttemptTimeout: time.Second,
			RobotsTotalTimeout:   5 * time.Second,
			MaxHTMLBodyBytes:     DefaultMaxHTMLBodyBytes,
		},
		&wg,
	)
	wg.Wait()
	close(results)

	result := <-results
	if result.Error != nil {
		t.Fatalf("robots disallow must be a normal outcome: %v", result.Error)
	}
	if result.Data.StatusCode != nil {
		t.Fatalf("blocked page must not have an HTTP status: %v", result.Data.StatusCode)
	}
	if result.Data.ScanStatus != scanStatusBlockedByRobots {
		t.Fatalf("unexpected scan status: %q", result.Data.ScanStatus)
	}
	if result.Data.RobotsOutcome != robotsOutcomeDisallowed || result.Data.RobotsAllowed {
		t.Fatalf("unexpected robots decision: outcome=%q allowed=%t", result.Data.RobotsOutcome, result.Data.RobotsAllowed)
	}
	if pageRequests.Load() != 0 {
		t.Fatalf("robots-disallowed page was fetched: requests=%d", pageRequests.Load())
	}
}

func TestValidateResolvedTargetIPsBlocksPrivateAndSpecialRanges(t *testing.T) {
	tests := []struct {
		name                string
		ips                 []netip.Addr
		allowPrivateTargets bool
		wantErr             bool
	}{
		{name: "public IPv4", ips: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
		{name: "loopback", ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, wantErr: true},
		{name: "private IPv4", ips: []netip.Addr{netip.MustParseAddr("10.0.0.5")}, wantErr: true},
		{name: "cgnat IPv4", ips: []netip.Addr{netip.MustParseAddr("100.64.0.10")}, wantErr: true},
		{name: "public IPv4 through well-known NAT64", ips: []netip.Addr{netip.MustParseAddr("64:ff9b::5db8:d822")}},
		{name: "loopback through well-known NAT64", ips: []netip.Addr{netip.MustParseAddr("64:ff9b::7f00:1")}, wantErr: true},
		{name: "private IPv4 through well-known NAT64", ips: []netip.Addr{netip.MustParseAddr("64:ff9b::a00:5")}, wantErr: true},
		{name: "local-use NAT64", ips: []netip.Addr{netip.MustParseAddr("64:ff9b:1::5db8:d822")}, wantErr: true},
		{name: "documentation IPv6", ips: []netip.Addr{netip.MustParseAddr("2001:db8::1")}, wantErr: true},
		{
			name:    "mixed DNS response",
			ips:     []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")},
			wantErr: true,
		},
		{
			name:                "private allowed by config",
			ips:                 []netip.Addr{netip.MustParseAddr("127.0.0.1")},
			allowPrivateTargets: true,
			wantErr:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResolvedTargetIPs("example.test", tt.ips, tt.allowPrivateTargets)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResolvedTargetIPs() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestNewHTTPTransportUsesSafeDialer(t *testing.T) {
	transport := newHTTPTransport(Config{
		Workers:             1,
		AllowPrivateTargets: false,
	}, time.Second)
	if transport.DialContext == nil {
		t.Fatalf("expected custom DialContext for transport-level SSRF protection")
	}
}

func TestNormalizeTargetURL(t *testing.T) {
	tests := []struct {
		name                string
		rawURL              string
		allowPrivateTargets bool
		wantErr             bool
	}{
		{name: "public https", rawURL: "https://example.com/path", wantErr: false},
		{name: "unsupported scheme", rawURL: "ftp://example.com/file", wantErr: true},
		{name: "localhost blocked", rawURL: "http://localhost:8080", wantErr: true},
		{name: "private ipv4 blocked", rawURL: "http://10.0.0.5", wantErr: true},
		{name: "credentials blocked", rawURL: "https://user:pass@example.com", wantErr: true},
		{name: "private allowed by config", rawURL: "http://127.0.0.1:8080", allowPrivateTargets: true, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeTargetURL(tt.rawURL, tt.allowPrivateTargets)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeTargetURL() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestStreamTargetURLsUsesClaimedBatches(t *testing.T) {
	records := []targetURLRecord{
		{ID: 1, URL: "https://example.com/one"},
		{ID: 2, URL: "ftp://example.com/unsupported"},
		{ID: 3, URL: "https://example.com/three"},
		{ID: 4, URL: "http://127.0.0.1/private"},
	}

	var batchLimits []int
	claimBatch := func(_ context.Context, limit int) ([]targetURLRecord, error) {
		if limit < 1 || limit > 2 {
			t.Fatalf("unexpected batch limit: %d", limit)
		}
		batchLimits = append(batchLimits, limit)

		if len(records) == 0 {
			return nil, nil
		}
		count := min(limit, len(records))
		batch := append([]targetURLRecord(nil), records[:count]...)
		records = records[count:]
		return batch, nil
	}

	jobs := make(chan AuditTarget, 1)
	invalidResults := make(chan Result, 2)
	collected := make(chan []string, 1)
	go func() {
		var urls []string
		for target := range jobs {
			urls = append(urls, target.RequestURL)
		}
		collected <- urls
	}()

	cfg := Config{TargetFingerprintKey: []byte(testTargetFingerprintKey)}
	summary := streamTargetURLs(context.Background(), 2, cfg, jobs, invalidResults, claimBatch)
	close(jobs)
	close(invalidResults)
	urls := <-collected

	if summary.Error != nil {
		t.Fatalf("streamTargetURLs returned error: %v", summary.Error)
	}
	if summary.Queued != 2 || summary.Skipped != 2 {
		t.Fatalf("unexpected stream summary: queued=%d skipped=%d", summary.Queued, summary.Skipped)
	}
	invalidCount := 0
	for result := range invalidResults {
		invalidCount++
		if result.Target.TargetID == 0 {
			t.Fatalf("invalid target result has no target ID")
		}
		if result.Data.ScanStatus != scanStatusFailed || result.Data.ErrorCode != errorCodeInvalidTargetURL {
			t.Fatalf("unexpected invalid target result: status=%q code=%q", result.Data.ScanStatus, result.Data.ErrorCode)
		}
	}
	if invalidCount != 2 {
		t.Fatalf("unexpected invalid target result count: %d", invalidCount)
	}
	if len(urls) != 2 || urls[0] != "https://example.com/one" || urls[1] != "https://example.com/three" {
		t.Fatalf("unexpected queued URLs: %#v", urls)
	}
	if len(batchLimits) < 3 {
		t.Fatalf("too few bounded claim calls: %#v", batchLimits)
	}
}

func TestCancellationWaitsForAllResultProducers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan AuditTarget)
	results := make(chan Result)
	streamDone := make(chan urlStreamSummary, 1)
	streamFinished := make(chan struct{})
	batchClaimed := make(chan struct{})
	claimCalls := 0
	claimBatch := func(_ context.Context, _ int) ([]targetURLRecord, error) {
		claimCalls++
		if claimCalls == 1 {
			close(batchClaimed)
			return []targetURLRecord{{ID: 1, URL: "ftp://example.com/unsupported"}}, nil
		}
		return nil, nil
	}

	go func() {
		defer close(jobs)
		defer close(streamFinished)
		streamDone <- streamTargetURLs(
			ctx,
			1,
			Config{Workers: 1, TargetFingerprintKey: []byte(testTargetFingerprintKey)},
			jobs,
			results,
			claimBatch,
		)
	}()

	var workers sync.WaitGroup
	resultsClosed := make(chan struct{})
	go func() {
		closeResultsAfterProducers(&workers, streamFinished, results)
		close(resultsClosed)
	}()

	<-batchClaimed
	select {
	case <-resultsClosed:
		t.Fatal("results closed while stream producer was still active")
	default:
	}

	cancel()
	streamSummary := <-streamDone
	if !errors.Is(streamSummary.Error, context.Canceled) {
		t.Fatalf("stream error = %v, want context cancellation", streamSummary.Error)
	}

	select {
	case <-resultsClosed:
	case <-time.After(time.Second):
		t.Fatal("results was not closed after all producers finished")
	}
	if _, open := <-results; open {
		t.Fatal("results channel remains open")
	}
}

func TestNextTargetClaimLimitUsesWorkersAndFreeQueueCapacity(t *testing.T) {
	jobs := make(chan AuditTarget, 6)
	for range 4 {
		jobs <- AuditTarget{}
	}

	if got, want := nextTargetClaimLimit(100, 3, jobs), 5; got != want {
		t.Fatalf("nextTargetClaimLimit() = %d, want %d", got, want)
	}
	if got, want := nextTargetClaimLimit(4, 3, jobs), 4; got != want {
		t.Fatalf("batch cap was not applied: got %d want %d", got, want)
	}
}

func TestHeartbeatMonitorStopsAfterConsecutiveFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	heartbeatErr := errors.New("heartbeat unavailable")
	var updates atomic.Int32
	fatalErrors := make(chan error, 1)
	done := startHeartbeatMonitor(
		ctx,
		time.Millisecond,
		3,
		func() error {
			attempt := updates.Add(1)
			if attempt == 2 {
				return nil
			}
			return heartbeatErr
		},
		func(err error) { fatalErrors <- err },
	)

	select {
	case err := <-fatalErrors:
		if !errors.Is(err, heartbeatErr) {
			t.Fatalf("fatal heartbeat error = %v, want wrapped test error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat monitor did not report the failure threshold")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat monitor did not stop after the failure threshold")
	}
	if got, want := updates.Load(), int32(5); got != want {
		t.Fatalf("heartbeat updates = %d, want %d", got, want)
	}
}

func TestWaitForStaleRecoveryContentionStopsAfterWriteBudget(t *testing.T) {
	contentionSince := time.Now().Add(-2 * time.Millisecond)
	err := waitForStaleRecoveryContention(
		context.Background(),
		Config{DBWriteTimeout: time.Millisecond},
		&contentionSince,
	)
	if err == nil || !strings.Contains(err.Error(), "database lock contention") {
		t.Fatalf("unexpected stale recovery contention result: %v", err)
	}
}

func TestWorkerFinishesInFlightTaskAndStopsBeforeNextTask(t *testing.T) {
	pageStarted := make(chan struct{}, 1)
	releasePage := make(chan struct{})
	var releaseOnce sync.Once
	var pageRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}

		pageRequests.Add(1)
		select {
		case pageStarted <- struct{}{}:
		default:
		}
		<-releasePage

		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head><title>Graceful shutdown test page</title></head><body><h1>Ready</h1></body></html>")
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releasePage) })
		server.Close()
	})

	schedulingCtx, stopScheduling := context.WithCancel(context.Background())
	operationCtx, cancelOperations := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancelOperations)

	firstTarget := newAuditTarget(targetURLRecord{ID: 1, URL: server.URL}, server.URL, []byte(testTargetFingerprintKey))
	secondTarget := newAuditTarget(targetURLRecord{ID: 2, URL: server.URL}, server.URL, []byte(testTargetFingerprintKey))
	jobs := make(chan AuditTarget, 2)
	jobs <- firstTarget
	jobs <- secondTarget
	close(jobs)
	results := make(chan Result, 2)

	var wg sync.WaitGroup
	wg.Add(1)
	go worker(
		schedulingCtx,
		operationCtx,
		1,
		jobs,
		results,
		server.Client(),
		server.Client(),
		newRobotsPolicyCache(time.Minute, 16),
		nil,
		Config{
			HTTPAttemptTimeout:   2 * time.Second,
			HTTPTotalTimeout:     5 * time.Second,
			RobotsAttemptTimeout: time.Second,
			RobotsTotalTimeout:   5 * time.Second,
			MaxHTMLBodyBytes:     DefaultMaxHTMLBodyBytes,
		},
		&wg,
	)

	select {
	case <-pageStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first page request did not start")
	}

	stopScheduling()
	releaseOnce.Do(func() { close(releasePage) })

	workerDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workerDone)
	}()
	select {
	case <-workerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not finish the in-flight task")
	}
	close(results)

	resultCount := 0
	for result := range results {
		resultCount++
		if result.Error != nil {
			t.Fatalf("in-flight task returned error: %v", result.Error)
		}
	}

	if resultCount != 1 {
		t.Fatalf("unexpected result count: got %d want 1", resultCount)
	}
	if pageRequests.Load() != 1 {
		t.Fatalf("worker started a new task after shutdown: requests=%d", pageRequests.Load())
	}
}

func TestGracefulShutdownGuardCancelsOperationsAfterTimeout(t *testing.T) {
	schedulingCtx, stopScheduling := context.WithCancel(context.Background())
	operationCtx, cancelOperations := context.WithCancel(context.Background())
	t.Cleanup(cancelOperations)

	processingDone := make(chan struct{})
	guardDone := guardGracefulShutdown(
		schedulingCtx,
		processingDone,
		20*time.Millisecond,
		cancelOperations,
	)
	stopScheduling()

	select {
	case <-guardDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown guard did not enforce its timeout")
	}
	if operationCtx.Err() == nil {
		t.Fatal("operation context was not canceled after shutdown timeout")
	}
}

func TestCanceledCompletionWaitsForLockedIncompleteTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	cfg := Config{
		DBWriteTimeout: 500 * time.Millisecond,
		RetryBaseDelay: time.Millisecond,
	}
	var mu sync.Mutex
	locked := true
	processed := false
	updateCalls := 0
	firstAttempt := make(chan struct{})
	var firstAttemptOnce sync.Once

	go func() {
		<-firstAttempt
		mu.Lock()
		locked = false
		mu.Unlock()
	}()

	err := drainIncompleteTargetBatches(
		ctx,
		cfg,
		"test canceled completion",
		func() (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			updateCalls++
			if locked {
				firstAttemptOnce.Do(func() { close(firstAttempt) })
				return 0, nil
			}
			if processed {
				return 0, nil
			}
			processed = true
			return 1, nil
		},
		func() (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return locked || !processed, nil
		},
	)
	if err != nil {
		t.Fatalf("drain incomplete target batches: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !processed {
		t.Fatal("completion returned before the locked target was processed")
	}
	if updateCalls < 2 {
		t.Fatalf("completion did not retry after lock contention: calls=%d", updateCalls)
	}
}
