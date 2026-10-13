package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
)

func TestURLSignalsPreserveIdentityWithoutSecrets(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-fingerprint-key")}
	a := secureURLSignals(SEOData{CanonicalURL: "https://example.com/file?token=fixture-A"}, cfg)
	b := secureURLSignals(SEOData{CanonicalURL: "https://example.com/file?token=fixture-B"}, cfg)
	if a.HTML.Fingerprints[0] == b.HTML.Fingerprints[0] || a.HTML.URL != b.HTML.URL {
		t.Fatal("redaction collision")
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "fixture-A") {
		t.Fatal("secret retained")
	}
	if secureURLReference(crawler.URLReference{URL: "https://example.com/#section"}, cfg, true).Valid {
		t.Fatal("fragment canonical accepted")
	}
	home := secureURLReference(crawler.URLReference{URL: "https://example.com/"}, cfg, true)
	other := secureURLReference(crawler.URLReference{URL: "https://example.com"}, cfg, true)
	if !referencesIntersect(home, other) {
		t.Fatal("equivalent homepage identities differ")
	}
	truncated := secureURLSignals(SEOData{CanonicalURL: "https://example.com/", CanonicalURLTruncated: true}, cfg)
	if truncated.HTML.Valid || len(truncated.HTML.Fingerprints) > 0 || !truncated.HTML.Truncated {
		t.Fatal("truncated canonical became a valid identity")
	}
}
