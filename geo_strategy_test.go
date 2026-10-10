package main

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestGEOStrategyEndpointsRequireAuthentication(t *testing.T) {
	app := testWebApp()
	id := newWebRunID()
	for _, path := range []string{"/api/geo/profiles", "/api/geo/profiles/" + id + "/history", "/api/geo/profiles/" + id + "/timeline", "/api/geo/profiles/" + id + "/changes", "/api/geo/profiles/" + id + "/observations", "/api/geo/reports/" + id + "/topics", "/api/geo/reports/" + id + "/competitors", "/api/geo/competitors/" + id} {
		w := httptest.NewRecorder()
		app.handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil))
		if w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestCompetitorInputsRemainBoundedAndSecretFree(t *testing.T) {
	input := competitorSetInput{ID: newWebRunID(), Competitors: []competitorInput{{URL: "competitor.example", Brand: "Перший"}, {URL: "https://other.example/article"}}}
	items, err := prepareCompetitors(&input, "example.com", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !items[0].Site || items[1].Site || input.MaxPages != 25 || input.FreshHours != 24 {
		t.Fatal(items, input)
	}
	for _, raw := range []string{"https://example.com/", "https://sub.example.com/", "http://127.0.0.1/", "https://user:pass@competitor.example/", "https://competitor.example/?X-Amz-Signature=secret"} {
		bad := competitorSetInput{ID: newWebRunID(), Competitors: []competitorInput{{URL: raw}, {URL: "https://other.example/"}}}
		if _, err := prepareCompetitors(&bad, "example.com", Config{}); err == nil {
			t.Fatalf("Прийнято %s", raw)
		}
	}
	input.MaxPages = 51
	if _, err := prepareCompetitors(&input, "example.com", Config{}); err == nil {
		t.Fatal("Не перевірено ліміт сторінок")
	}
}

func TestVisibilityChangesUseFingerprintAndExplicitZero(t *testing.T) {
	id := int64(1)
	ordinal := 1
	zero, positive := int64(0), int64(42)
	a := identifiedObservation{Record: geo.VisibilityRecord{Scope: "url", URL: "https://example.com/file?token=[REDACTED]", TargetID: &id, Impressions: &zero}, Fingerprint: bytes.Repeat([]byte{1}, 32), KeyID: "v1"}
	b := a
	b.Record.Impressions = &positive
	if changes := compareVisibilityObservations([]identifiedObservation{a}, []identifiedObservation{b}); len(changes) != 1 || changes[0].Query != "" {
		t.Fatal(changes)
	}
	b.Fingerprint = bytes.Repeat([]byte{2}, 32)
	if changes := compareVisibilityObservations([]identifiedObservation{a}, []identifiedObservation{b}); len(changes) != 0 {
		t.Fatal("Redaction-колізія: ", changes)
	}
	b = a
	b.KeyID = "v2"
	b.Record.Impressions = &positive
	if changes := compareVisibilityObservations([]identifiedObservation{a}, []identifiedObservation{b}); len(changes) != 0 {
		t.Fatal("Ротацію ключа проігноровано")
	}
	b = a
	b.Record.Impressions = nil
	if changes := compareVisibilityObservations([]identifiedObservation{a}, []identifiedObservation{b}); len(changes) != 0 {
		t.Fatal("Невідоме перетворено на нуль")
	}
	a = identifiedObservation{Record: geo.VisibilityRecord{Scope: "query", Query: "Chargeback protection", ReportOrdinal: &ordinal, Impressions: &zero}}
	b = a
	b.Record.Query = "chargeback protection"
	b.Record.Impressions = &positive
	if changes := compareVisibilityObservations([]identifiedObservation{a}, []identifiedObservation{b}); len(changes) != 1 {
		t.Fatal(changes)
	}
}
