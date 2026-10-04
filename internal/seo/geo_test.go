package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
