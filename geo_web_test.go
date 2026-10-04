package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestGEOStorageSanitizesNullWithoutChangingSEOData(t *testing.T) {
	raw := "до\x00після https://example.com/?token=private-value " + strings.Repeat("ї", 2200)
	signals := &geo.Signals{Excerpt: raw, Headings: raw, FirstParagraph: raw}
	data := SEOData{Title: "Заголовок", WordCount: 7, GEO: signals}
	got := sanitizeSEODataForStorage(data)
	if got.Title != data.Title || got.WordCount != data.WordCount || signals.Excerpt != raw || got.GEO == signals {
		t.Fatal("Очищення GEO змінило SEO-метрики або початкові сигнали")
	}
	for limit, value := range map[int]string{2000: got.GEO.Excerpt, 800: got.GEO.Headings, 600: got.GEO.FirstParagraph} {
		if strings.ContainsRune(value, 0) || strings.Contains(value, "private-value") ||
			!strings.HasPrefix(value, "до\uFFFDпісля ") || utf8.RuneCountInString(value) != limit {
			t.Fatalf("Некоректне очищення GEO-поля з лімітом %d", limit)
		}
	}
}

func TestGEOUsesExistingAPISecurity(t *testing.T) {
	h := testWebApp().handler()
	for _, path := range []string{"/api/geo/sources", "/api/geo/reports", "/api/geo/reports/00000000-0000-4000-8000-000000000001"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil))
		if w.Code != 401 {
			t.Fatalf("API без авторизації: %d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/geo/reports", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testWebToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("Відсутній захист Origin")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080/geo", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Готовність до GEO") {
		t.Fatal("Український інтерфейс не завантажується")
	}
}

func TestGEOManualCitationValidationAndRedaction(t *testing.T) {
	c := geoCitation{Engine: "chatgpt", Citation: "yes", BrandMention: "unknown", EvidenceURL: "https://example.com/share?token=secret#fragment", Note: "Доказ https://example.com/?signature=private"}
	if err := validateGEOCitation(&c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.EvidenceURL, "secret") || strings.Contains(c.Note, "private") {
		t.Fatal("Не очищено параметри URL")
	}
	for _, raw := range []string{"javascript:alert(1)", "https://user:pass@example.com", "file:///secret"} {
		c.EvidenceURL = raw
		if err := validateGEOCitation(&c); err == nil {
			t.Fatal("Небезпечне посилання прийнято")
		}
	}
}

func TestGEOCSVNeutralizesFormulaAndPreservesUnicode(t *testing.T) {
	w := httptest.NewRecorder()
	writeGEOCSV(w, geoReport{ID: "test"}, []geoRow{{Result: geo.Result{Query: "=HYPERLINK(1)", Intent: "unknown", Level: "unknown", Gaps: []string{"Перевірте сторінку"}}}})
	if !strings.Contains(w.Body.String(), "'=HYPERLINK(1)") || !strings.Contains(w.Body.String(), "Перевірте сторінку") {
		t.Fatal("Некоректний CSV")
	}
}
