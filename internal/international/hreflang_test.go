package international

import "testing"

func TestLanguageCodes(t *testing.T) {
	for _, v := range []string{"en", "uk-UA", "es-ES", "pl", "zh-Hans", "zh-Hant-TW", "x-default"} {
		if !ValidLanguage(v) {
			t.Errorf("valid %s rejected", v)
		}
	}
	for _, v := range []string{"en-UK", "en-EU", "en-001", "eng", "zz", "uk_UA", "en-ZZ", "en-US-extra", "x", "en-QQ", "en-Private"} {
		if ValidLanguage(v) {
			t.Errorf("invalid %s accepted", v)
		}
	}
}

func TestHreflangDiagnostics(t *testing.T) {
	yes, no, ok, missing, redirect := true, false, 200, 404, 301
	self := Alternate{Language: "en", URL: "https://example.com/en", Fingerprint: "en", Absolute: true, ValidURL: true}
	other := Alternate{Language: "es", URL: "https://example.com/es", Fingerprint: "es", Absolute: true, ValidURL: true}
	for _, tc := range []struct {
		name  string
		other Observation
		code  string
	}{
		{"reciprocal", Observation{&ok, "completed", &yes, &yes, "en"}, "missing_x_default"},
		{"missing_return", Observation{&ok, "completed", &no, &yes, "es"}, "missing_return_link"},
		{"canonical", Observation{&ok, "completed", &yes, &no, "en"}, "alternate_canonical_conflict"},
		{"http", Observation{&missing, "completed", nil, nil, ""}, "alternate_http"},
		{"redirect", Observation{&redirect, "redirect", nil, nil, ""}, "alternate_http"},
		{"incomplete_html", Observation{&ok, "failed", nil, nil, ""}, "alternate_not_checked"},
		{"not_checked", Observation{}, "alternate_not_checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Page{Fingerprint: "en", HTML: &Evidence{Links: []Alternate{self, other}, Complete: true}, Observations: map[string]Observation{"en": {&ok, "completed", &yes, &yes, "en"}, "es": tc.other}}
			Diagnose(&p)
			found := false
			for _, i := range p.Issues {
				found = found || i.Code == tc.code
			}
			if !found {
				t.Fatalf("missing %s: %+v", tc.code, p)
			}
		})
	}
	p := Page{Fingerprint: "en", HTML: &Evidence{Links: []Alternate{self}, Complete: true}, Sitemap: &Evidence{Links: []Alternate{other}, Complete: true}}
	Diagnose(&p)
	found := false
	for _, i := range p.Issues {
		found = found || i.Code == "html_sitemap_mismatch"
	}
	if !found {
		t.Fatal("different channels accepted")
	}
	p.HTML.Complete = false
	Diagnose(&p)
	for _, i := range p.Issues {
		if i.Code == "html_sitemap_mismatch" || (i.Source == "html" && i.Code == "missing_x_default") {
			t.Fatal("absence inferred from incomplete evidence")
		}
	}
}

func TestRegionalCanonicalNeedsReviewNotAutomaticError(t *testing.T) {
	e := &Evidence{Complete: true, Links: []Alternate{{Language: "en-US", Fingerprint: "us", Absolute: true, ValidURL: true}, {Language: "en-GB", Fingerprint: "gb", Absolute: true, ValidURL: true}, {Language: "pl", Fingerprint: "pl", Absolute: true, ValidURL: true}}}
	if canonicalSeverity("gb", "us", e) != "review" {
		t.Fatal("same-language regional canonical treated as invalid")
	}
	if canonicalSeverity("pl", "us", e) != "error" {
		t.Fatal("known language conflict not diagnosed")
	}
	if canonicalSeverity("gb", "unknown", e) != "review" {
		t.Fatal("unknown canonical language treated as proven error")
	}
}
