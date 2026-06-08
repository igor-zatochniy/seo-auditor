package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
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

	data, err := parsePage(resp, "https://example.com/page", DefaultMaxHTMLBodyBytes)
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
	if data.TotalImages != 3 || data.ImagesMissingAlt != 2 {
		t.Fatalf("unexpected image audit: total=%d missing=%d", data.TotalImages, data.ImagesMissingAlt)
	}
	if data.MetaRobots != "index,follow" || data.XRobotsTag != "index, follow" {
		t.Fatalf("unexpected robots metadata: meta=%q header=%q", data.MetaRobots, data.XRobotsTag)
	}
	if data.WordCount == 0 {
		t.Fatalf("expected non-zero word count")
	}
}

func TestParsePageRejectsOversizedHTML(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("abcdef")),
	}

	_, err := parsePage(resp, "https://example.com", 5)
	if err == nil {
		t.Fatalf("expected body size limit error")
	}
}

func TestIsSelfCanonical(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		target    string
		want      bool
	}{
		{name: "empty canonical is self", canonical: "", target: "https://example.com/page", want: true},
		{name: "relative canonical", canonical: "/page", target: "https://example.com/page", want: true},
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
