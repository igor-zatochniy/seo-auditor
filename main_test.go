package main

import (
	"context"
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

func TestStorageSanitizerTruncatesOversizedHTMLMetadata(t *testing.T) {
	longTitle := strings.Repeat("T", storageTitleMaxRunes+25)
	longH1 := strings.Repeat("H", storageH1MaxRunes+25)
	longOGTitle := strings.Repeat("O", storageTitleMaxRunes+25)
	longTwitterCard := strings.Repeat("C", storageTwitterCardMaxRunes+25)
	longCanonical := "https://example.com/" + strings.Repeat("canonical", 260)
	longRobots := strings.Repeat("noindex,", 40)
	html := `<!doctype html>
<html>
<head>
  <title>` + longTitle + `</title>
  <link rel="canonical" href="` + longCanonical + `">
  <meta name="robots" content="` + longRobots + `">
  <meta property="og:title" content="` + longOGTitle + `">
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

	data, err := parsePage(resp, "https://example.com/page", DefaultMaxHTMLBodyBytes)
	if err != nil {
		t.Fatalf("parsePage returned error: %v", err)
	}
	if data.TitleStatus != "Too Long" || utf8.RuneCountInString(data.Title) != storageTitleMaxRunes+25 {
		t.Fatalf("parser did not preserve oversized title before storage: status=%q length=%d", data.TitleStatus, utf8.RuneCountInString(data.Title))
	}

	stored := sanitizeSEODataForStorage(data)
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

	_, err := parsePage(resp, "https://example.com", 5)
	if err == nil {
		t.Fatalf("expected body size limit error")
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

	data, err := parsePage(resp, "https://example.com/missing", DefaultMaxHTMLBodyBytes)
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

func TestParsePageDecodesDeclaredCharset(t *testing.T) {
	body := []byte("<html><head><title>caf\xe9 audit</title></head><body><h1>caf\xe9</h1></body></html>")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/html; charset=iso-8859-1"},
		},
		Body: io.NopCloser(strings.NewReader(string(body))),
	}

	data, err := parsePage(resp, "https://example.com", DefaultMaxHTMLBodyBytes)
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

func TestStreamTargetURLsUsesKeysetBatches(t *testing.T) {
	records := []targetURLRecord{
		{ID: 1, URL: "https://example.com/one"},
		{ID: 2, URL: "ftp://example.com/unsupported"},
		{ID: 3, URL: "https://example.com/three"},
		{ID: 4, URL: "http://127.0.0.1/private"},
	}

	var afterIDs []int64
	fetchBatch := func(_ context.Context, afterID, highWatermark int64, limit int) ([]targetURLRecord, error) {
		if highWatermark != 4 {
			t.Fatalf("unexpected high watermark: %d", highWatermark)
		}
		if limit != 2 {
			t.Fatalf("unexpected batch limit: %d", limit)
		}
		afterIDs = append(afterIDs, afterID)

		batch := make([]targetURLRecord, 0, limit)
		for _, record := range records {
			if record.ID <= afterID || record.ID > highWatermark {
				continue
			}
			batch = append(batch, record)
			if len(batch) == limit {
				break
			}
		}
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
	summary := streamTargetURLs(context.Background(), 4, 2, cfg, jobs, invalidResults, fetchBatch)
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
	if len(afterIDs) != 2 || afterIDs[0] != 0 || afterIDs[1] != 2 {
		t.Fatalf("unexpected keyset positions: %#v", afterIDs)
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
