// Package performance описує лише локальні лабораторні виміри, не польові CWV.
package performance

import "math"

type Result struct {
	Status     string   `json:"status"`
	LCPMS      *float64 `json:"lcp_ms"`
	CLS        *float64 `json:"cls"`
	LCPRating  string   `json:"lcp_rating"`
	CLSRating  string   `json:"cls_rating"`
	ObservedMS int64    `json:"observed_ms"`
	Requests   int      `json:"requests"`
	Blocked    int      `json:"blocked"`
	Error      string   `json:"error,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

func Rating(value *float64, good, poor float64) string {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return "unknown"
	}
	if *value <= good {
		return "good"
	}
	if *value <= poor {
		return "needs_improvement"
	}
	return "poor"
}

func (r *Result) Classify() {
	r.LCPRating, r.CLSRating = Rating(r.LCPMS, 2500, 4000), Rating(r.CLS, 0.1, 0.25)
	if r.LCPRating == "unknown" {
		r.LCPMS = nil
	}
	if r.CLSRating == "unknown" {
		r.CLS = nil
	}
	if r.Status != "failed" && (r.LCPRating == "unknown" || r.CLSRating == "unknown" || r.Blocked > 0 || len(r.Warnings) > 0) {
		r.Status = "partial"
	}
}
