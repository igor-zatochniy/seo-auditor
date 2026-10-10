package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
)

func TestSitemapHreflangNamespaceAndOrder(t *testing.T) {
	x := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:x="http://www.w3.org/1999/xhtml"><url><x:link rel="alternate" hreflang="en" href="https://example.com/en"/><loc>https://example.com/en</loc><x:link rel="alternate" hreflang="uk" href="https://example.com/uk"/><link rel="alternate" hreflang="pl" href="https://example.com/pl"/></url></urlset>`
	e := map[string]*international.Evidence{}
	urls, index, limited, err := parseSitemap(context.Background(), strings.NewReader(x), 10, e)
	if err != nil || index || limited || len(urls) != 1 || len(e[urls[0]].Links) != 2 {
		t.Fatalf("err=%v e=%+v", err, e)
	}
}

func TestInternationalSecretsRemainDistinct(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("test-only-key")}
	e := &international.Evidence{Complete: true, Links: []international.Alternate{
		{Language: "en", URL: "https://example.com/?X-Amz-Signature=secret-alpha", Absolute: true, ValidURL: true},
		{Language: "es", URL: "https://example.com/?X-Amz-Signature=secret-beta", Absolute: true, ValidURL: true},
	}}
	v := secureInternationalEvidence(e, "https://example.com/?token=private-canonical", cfg)
	b, _ := json.Marshal(v)
	for _, secret := range []string{"secret-alpha", "secret-beta", "private-canonical"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret persisted")
		}
	}
	if v.Links[0].Fingerprint == v.Links[1].Fingerprint || v.Links[0].URL != v.Links[1].URL {
		t.Fatalf("redaction/identity mismatch: %+v", v)
	}
	if !strings.Contains(e.Links[0].URL, "secret-alpha") {
		t.Fatal("input evidence mutated")
	}
}

func TestInternationalEmptyLinksSerializeAsArray(t *testing.T) {
	v := secureInternationalEvidence(&international.Evidence{Complete: true}, "", Config{})
	b, err := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"links":[]`) {
		t.Fatalf("empty links must be a JSON array: %s, %v", b, err)
	}
}
