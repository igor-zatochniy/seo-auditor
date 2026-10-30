package main

import (
	"math"
	"testing"
)

func TestChooseVisibilityMatchIsExplicitAndDeterministic(t *testing.T) {
	a := visibilityTargetCandidate{id: 1, fingerprint: "one", keyID: "k"}
	b := visibilityTargetCandidate{id: 2, fingerprint: "two", keyID: "k"}
	tests := []struct {
		name      string
		exact     []visibilityTargetCandidate
		canonical []visibilityTargetCandidate
		want      string
		id        int64
	}{
		{name: "exact takes precedence", exact: []visibilityTargetCandidate{a}, canonical: []visibilityTargetCandidate{b}, want: "exact", id: 1},
		{name: "duplicate paths to same target", exact: []visibilityTargetCandidate{a, a}, want: "exact", id: 1},
		{name: "multiple exact targets", exact: []visibilityTargetCandidate{a, b}, want: "ambiguous"},
		{name: "canonical match", canonical: []visibilityTargetCandidate{a}, want: "canonical_match", id: 1},
		{name: "multiple canonical targets", canonical: []visibilityTargetCandidate{a, b}, want: "ambiguous"},
		{name: "not found", want: "unmatched"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, got := chooseVisibilityMatch(test.exact, test.canonical)
			if got != test.want || (test.id != 0 && candidate.id != test.id) {
				t.Fatalf("match=(%+v,%q), want status=%q target=%d", candidate, got, test.want, test.id)
			}
		})
	}
}

func TestVisibilityCandidateAccumulatorIsBoundedAndPreservesAmbiguity(t *testing.T) {
	var items []visibilityTargetCandidate
	for i := range 10000 {
		items = appendVisibilityCandidate(items, visibilityTargetCandidate{id: int64(i/2 + 1)})
		if len(items) > 2 {
			t.Fatalf("unbounded candidates: %d", len(items))
		}
	}
	if _, match := chooseVisibilityMatch(items, nil); match != "ambiguous" {
		t.Fatalf("ambiguity lost: %q", match)
	}
}

func TestSignedQueryValuesRemainDistinctForMatching(t *testing.T) {
	secret := []byte("test-only-fingerprint-key")
	a := fingerprintURL(secret, "https://example.com/report?token=AAAA")
	b := fingerprintURL(secret, "https://example.com/report?token=BBBB")
	if string(a) == string(b) {
		t.Fatal("different query values unexpectedly share a URL identity")
	}
}

func TestImpactClickEstimateUsesSampleConfidenceAndNeverPredictsGain(t *testing.T) {
	impressions := int64(20000)
	actual, baseline := .02, .05
	value, confidence, _ := estimateImpactClicks(&impressions, &actual, &baseline, 30, 10000)
	if value == nil || math.Abs(*value-600) > 1e-9 || confidence != "high" {
		t.Fatalf("estimate=%v confidence=%q, want 600/high", value, confidence)
	}
	value, confidence, _ = estimateImpactClicks(&impressions, &baseline, &actual, 10, 1000)
	if value == nil || *value != 0 || confidence != "medium" {
		t.Fatalf("non-opportunity estimate=%v confidence=%q, want 0/medium", value, confidence)
	}
	_, confidence, _ = estimateImpactClicks(&impressions, &actual, &baseline, 1, 20)
	if confidence != "low" {
		t.Fatalf("small baseline confidence=%q, want low", confidence)
	}
	value, confidence, _ = estimateImpactClicks(nil, &actual, &baseline, 30, 10000)
	if value != nil || confidence != "unknown" {
		t.Fatalf("missing observations got value=%v confidence=%q", value, confidence)
	}
}

func TestPositionBuckets(t *testing.T) {
	for _, test := range []struct {
		position float64
		want     string
	}{{1, "1-3"}, {3, "1-3"}, {4, "4-6"}, {7, "7-10"}, {11, "11-20"}, {21, "21+"}} {
		if got := positionBucket(&test.position); got != test.want {
			t.Errorf("position %v bucket %q, want %q", test.position, got, test.want)
		}
	}
	if got := positionBucket(nil); got != "" {
		t.Fatalf("missing position bucket %q", got)
	}
}
