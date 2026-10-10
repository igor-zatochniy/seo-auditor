package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHreflangCollectedWithoutRendering(t *testing.T) {
	d, err := ParsePage(&http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<head><base href="https://example.com/"><link rel="alternate" hreflang="en" href="https://example.com/en"><link rel="alternate" hreflang="es" href="/es"><template><link rel="alternate" hreflang="de" href="/de"></template></head><body><link rel="alternate" hreflang="pl" href="/pl"></body>`))}, "https://example.com/en", 1<<20, 1<<18)
	if err != nil {
		t.Fatal(err)
	}
	if d.International == nil || len(d.International.Links) != 2 || !d.International.Complete {
		t.Fatalf("%+v", d.International)
	}
	if d.International.Links[1].Absolute || d.International.Links[1].URL != "https://example.com/es" {
		t.Fatalf("relative URL evidence lost: %+v", d.International.Links[1])
	}
}

func TestHreflangEvidenceBudget(t *testing.T) {
	h := `<head>` + strings.Repeat(`<link rel="alternate" hreflang="en" href="https://example.com/en">`, 100) + `</head>`
	d, err := ParsePage(&http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(h))}, "https://example.com", 1<<20, 1<<18)
	if err != nil || d.International == nil || len(d.International.Links) != 64 || d.International.Complete {
		t.Fatalf("budget err=%v evidence=%+v", err, d.International)
	}
}
