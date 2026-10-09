package performance

import (
	"math"
	"testing"
)

func TestRatingsAndMissingMeasurements(t *testing.T) {
	for _, tc := range []struct {
		value, good, poor float64
		want              string
	}{{2500, 2500, 4000, "good"}, {2500.1, 2500, 4000, "needs_improvement"}, {4000, 2500, 4000, "needs_improvement"}, {4001, 2500, 4000, "poor"}, {0.1, 0.1, 0.25, "good"}, {0.25, 0.1, 0.25, "needs_improvement"}, {0.251, 0.1, 0.25, "poor"}, {-1, 0.1, 0.25, "unknown"}, {math.NaN(), 0.1, 0.25, "unknown"}} {
		if got := Rating(&tc.value, tc.good, tc.poor); got != tc.want {
			t.Fatalf("rating %v: %s", tc.value, got)
		}
	}
	r := Result{Status: "completed"}
	r.Classify()
	if r.Status != "partial" || r.LCPRating != "unknown" || r.CLSRating != "unknown" {
		t.Fatalf("Missing metrics became success: %+v", r)
	}
	r.Status = "failed"
	r.Classify()
	if r.Status != "failed" {
		t.Fatal("lost failure")
	}
}
