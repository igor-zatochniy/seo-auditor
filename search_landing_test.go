package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const searchHeader = "Date,Query,Page,Country,Device,Impressions,Clicks,Position\n"

func TestSearchCSVPreservesIdentityWithoutSecrets(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-fingerprint-key")}
	raw := searchHeader + "2026-10-01,Chargeback prevention,https://example.com/file?token=fixture-a,US,desktop,100,5,2.5\n" +
		"2026-10-01,Chargeback prevention,https://example.com/file?token=fixture-b,US,desktop,50,1,5\n"
	rows, err := parseSearchCSV(context.Background(), cfg, "https://example.com", raw)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].URL != rows[1].URL || string(rows[0].Fingerprint) == string(rows[1].Fingerprint) || strings.Contains(rows[0].URL, "fixture-a") {
		t.Fatal("URL identity or redaction lost")
	}
	if rows[0].QueryKey != "chargeback prevention" || rows[0].Country != "USA" || *rows[0].Position != 2.5 {
		t.Fatal("dimensions lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = parseSearchCSV(ctx, cfg, "https://example.com", raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSearchQueryIdentityIsNotLexicalMatching(t *testing.T) {
	if searchQueryKey("C++") == searchQueryKey("C#") || searchQueryKey("café") == searchQueryKey("cafe") {
		t.Fatal("different GSC queries were merged")
	}
	if searchQueryKey("  Chargeback   Prevention ") != searchQueryKey("chargeback prevention") {
		t.Fatal("case/space normalization lost")
	}
	a, _ := searchCountry("US")
	b, _ := searchCountry("usa")
	if a != "USA" || a != b {
		t.Fatal("country code aliases differ")
	}
}

func TestSearchCSVRejectsInvalidAndConflictingRows(t *testing.T) {
	cfg := Config{TargetFingerprintKey: []byte("local-development-only-fingerprint-key")}
	valid := "2026-10-01,test,https://example.com/a,USA,mobile,100,5,2\n"
	for name, raw := range map[string]string{
		"duplicate":         searchHeader + valid + valid,
		"origin":            searchHeader + strings.Replace(valid, "example.com", "other.example", 1),
		"nan":               searchHeader + strings.Replace(valid, ",5,2", ",5,NaN", 1),
		"clicks":            searchHeader + strings.Replace(valid, ",100,5", ",1,5", 1),
		"date":              searchHeader + strings.Replace(valid, "2026-10-01", "2026-02-30", 1),
		"missing_dimension": "Date,Query,Page,Impressions,Clicks\n2026-10-01,test,https://example.com/a,1,0\n",
		"bad_device":        searchHeader + strings.Replace(valid, "mobile", "all", 1),
		"oversize":          strings.Repeat("x", maxSearchCSVBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSearchCSV(context.Background(), cfg, "https://example.com", raw); err == nil {
				t.Fatal("accepted invalid data")
			}
		})
	}
}

func TestSearchOriginsAndStructurePrefixes(t *testing.T) {
	a, e := searchOrigin("https://bücher.de/page")
	if e != nil || a != "https://xn--bcher-kva.de" {
		t.Fatalf("IDN=%s %v", a, e)
	}
	b, e := searchOrigin("https://example.com:443/a")
	if e != nil || b != "https://example.com" {
		t.Fatalf("port=%s %v", b, e)
	}
	for _, v := range []string{"products/", "//example.com/", "/blog/?x=1", "/bad\n/"} {
		if _, e = structurePrefix(v); e == nil {
			t.Errorf("accepted %q", v)
		}
	}
	for _, v := range []string{"/", "/blog/", "/pl/products/"} {
		if got, e := structurePrefix(v); e != nil || got != v {
			t.Errorf("prefix=%s %v", got, e)
		}
	}
}
