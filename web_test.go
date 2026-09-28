package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/igor-zatochniy/seo-auditor/internal/config"
)

const testWebToken = "test-only-web-access-token-not-a-secret"

func testWebApp() *webServer {
	return &webServer{web: appconfig.WebConfig{Addr: "127.0.0.1:8080", MaxURLs: 2, AccessToken: testWebToken},
		cfg:     Config{DBFetchTimeout: time.Second, FinalizationTimeout: time.Second, ReportExportTimeout: time.Second},
		queries: make(chan struct{}, 3), submissions: make(chan struct{}, 1)}
}

func TestParseWebURLs(t *testing.T) {
	urls, err := parseWebURLs("\n https://example.com/a#one\r\nhttps://example.com/a#two\nhttps://example.com/b\n", 2, false)
	if err != nil || len(urls) != 2 || urls[0] != "https://example.com/a" {
		t.Fatalf("URLs=%v error=%v", urls, err)
	}
	for _, s := range []string{"", "\n \n", "ftp://example.com", "https://user:pass@example.com", "http://127.0.0.1/x", "https://example.com/" + strings.Repeat("x", 2048), "https://example.com/a\nhttps://example.com/b\nhttps://example.com/c"} {
		if _, err := parseWebURLs(s, 2, false); err == nil {
			t.Fatalf("accepted invalid input %q", s[:min(len(s), 80)])
		}
	}
	if _, err := parseWebURLs("http://127.0.0.1/test", 1, true); err != nil {
		t.Fatal(err)
	}
}

func TestWebSecurityAndJSON(t *testing.T) {
	app := testWebApp()
	h := app.handler()
	request := func(method, path, body, origin, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-SEO-Auditor-Request", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method, path, body, origin, token string
		want                              int
	}{
		{"GET", "/healthz", "", "", "", 200}, {"GET", "/readyz", "", "", "", 503},
		{"GET", "/api/schema", "", "", "", 401}, {"GET", "/api/schema", "", "", testWebToken, 200},
		{"POST", "/api/audits", `{"urls":"https://example.com"}`, "https://evil.example", testWebToken, 403},
		{"POST", "/api/audits", `{`, "http://127.0.0.1:8080", testWebToken, 400},
		{"POST", "/api/audits", `{"urls":"https://user:secret@example.com"}`, "http://127.0.0.1:8080", testWebToken, 400},
		{"POST", "/api/audits", `{"urls":"` + strings.Repeat("x", 10000) + `"}`, "http://127.0.0.1:8080", testWebToken, 413},
	} {
		w := request(tc.method, tc.path, tc.body, tc.origin, tc.token)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("security headers missing")
		}
		if tc.want >= 400 {
			var data map[string]any
			if json.Unmarshal(w.Body.Bytes(), &data) != nil || data["error"] == nil {
				t.Fatal("unstable error envelope")
			}
		}
		if strings.Contains(w.Body.String(), "secret@example") {
			t.Fatal("secret reflected in error")
		}
	}
	w := request("POST", "/api/session", "{}", "http://127.0.0.1:8080", testWebToken)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly || w.Result().Cookies()[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie not protected")
	}
	r := httptest.NewRequest("GET", "http://evil.example/api/schema", nil)
	r.Header.Set("Authorization", "Bearer "+testWebToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("DNS rebinding host accepted")
	}
}

func TestWebQueryValidation(t *testing.T) {
	for _, raw := range []string{"limit=201", "limit=0", "after=nope", "filter=DROP+TABLE", "sort=safe_url%3BDROP+TABLE", "search=" + strings.Repeat("a", 513)} {
		v, _ := url.ParseQuery(raw)
		if _, err := parseResultQuery(v); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	q, err := parseResultQuery(url.Values{"after": {"-20"}, "filter": {"title_missing"}, "limit": {"200"}})
	if err != nil || q.After != -20 || q.Limit != 200 {
		t.Fatalf("%+v %v", q, err)
	}
	if _, err := decodeHistoryCursor("bad"); err == nil {
		t.Fatal("bad cursor accepted")
	}
	for _, field := range reportFields {
		if strings.Contains(field.Key, "fingerprint") || field.Key == "request_url" {
			t.Fatal("private field in schema")
		}
	}
}

func TestFullReportAndCSVSecurity(t *testing.T) {
	private := reportRecord{"title": "Visit https://example.com/?token=private-test", "safe_url": "https://example.com/?id=private-id"}
	encoded, _ := json.Marshal(sanitizeReportRecord(private))
	if strings.Contains(string(encoded), "private-test") || strings.Contains(string(encoded), "private-id") {
		t.Fatal("text URL redaction failed")
	}
	record := reportRecord{"safe_url": "https://example.com/", "title": `<script>alert("x")</script>`, "description": "=HYPERLINK(\"https://evil.example\")", "target_id": json.Number("9007199254740993"), "canonical_url": "https://example.com/", "title_truncated": true}
	visit := func(fn func(reportRecord) error) error { return fn(record) }
	var output bytes.Buffer
	if err := renderFullReport(&output, webRun{ID: "test"}, webAnalytics{}, visit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "<script>alert") || !strings.Contains(output.String(), "&lt;script&gt;") || !strings.Contains(output.String(), "9007199254740993") {
		t.Fatal("unsafe or incomplete HTML")
	}
	output.Reset()
	if err := writeCSVReport(&output, visit); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&output).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatal(err)
	}
	for i, key := range rows[0] {
		if key == "description" && !strings.HasPrefix(rows[1][i], "'=") {
			t.Fatal("formula injection")
		}
	}
	for _, v := range []string{"=1", "+2", "-3", "@SUM(A1)", "  =1", "\t=1", "\ufeff=1"} {
		if !strings.HasPrefix(safeCSVCell(v), "'") {
			t.Fatalf("unsafe CSV %q", v)
		}
	}
}

func TestAuditManagerConcurrentCancelAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := newAuditManager(ctx, nil, Config{})
	m.active = "active"
	m.cancel = cancel
	m.done = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.cancelRun("active")
			if _, err := m.start(context.Background(), nil, ""); err != errAuditBusy {
				t.Errorf("start allowed: %v", err)
			}
		}()
	}
	go func() { <-ctx.Done(); close(m.done) }()
	m.shutdown()
	wg.Wait()
}
