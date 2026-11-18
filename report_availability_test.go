package main

import (
	"database/sql"
	"strings"
	"testing"
)

func TestUnparsedHTMLMetricsRemainUnknown(t *testing.T) {
	r := sanitizeReportRecord(reportRecord{"html_parsed": false, "h1_count": 0, "has_json_ld": false, "has_viewport": false, "is_self_canonical": false, "status_code": 403, "word_count": 0, "x_robots_tag": "noindex"})
	for _, key := range []string{"h1_count", "has_json_ld", "has_viewport", "word_count", "is_self_canonical"} {
		if r[key] != nil {
			t.Fatalf("%s=%v", key, r[key])
		}
	}
	if r["status_code"] != 403 || r["x_robots_tag"] != "noindex" {
		t.Fatal("HTTP evidence changed")
	}
	for _, d := range recordDetails(r) {
		if isHTMLReportFieldKey(d.Label) && !strings.Contains(d.Value, "Не перевірено") {
			t.Fatal(d)
		}
	}
	row := newAuditReportRow("https://example.com/", sql.NullInt32{Int32: 403, Valid: true}, scanStatusCompleted, "", "", "", 0, 0, 0, 0, "", "", true, "allowed", 0, 0, "", "")
	if row.HTMLParsed || row.Title != "Не перевірено" || row.Links != "Не перевірено" {
		t.Fatal(row)
	}
}

func isHTMLReportFieldKey(label string) bool {
	for _, f := range reportFields {
		if f.Label == label && isHTMLReportField(f.Key) {
			return true
		}
	}
	return false
}

func TestUncheckedRobotsIsNotReportedAsBlocked(t *testing.T) {
	for _, outcome := range []string{robotsOutcomeNotChecked, robotsOutcomeUnavailable} {
		r := sanitizeReportRecord(reportRecord{"robots_outcome": outcome, "robots_allowed": false})
		if r["robots_allowed"] != nil {
			t.Fatal("unknown robots policy became blocked", r)
		}
		row := newAuditReportRow("https://example.com/", sql.NullInt32{}, scanStatusFailed, "", "", "", 0, 0, 0, 0, "", "", false, outcome, 0, 0, "", "")
		if !strings.Contains(row.Robots, "Доступ: не перевірено") {
			t.Fatal(row.Robots)
		}
	}
}

func TestUncheckedLinkLookupUsesStableRunParameter(t *testing.T) {
	for _, rendering := range []bool{false, true} {
		query := reportSelectSQL(rendering)
		if strings.Contains(query, "e.run_id=r.run_id") || !strings.Contains(query, "dst.run_id=$1") || !strings.Contains(query, "dr.run_id=$1") {
			t.Fatal("run lookup must not be rebuilt for every result row")
		}
	}
	if strings.Contains(resultFilters["unchecked_internal"], "e.run_id=r.run_id") {
		t.Fatal("filter must use the same stable run parameter")
	}
}

func TestFullReportKeepsMetricsCollapsedAndEscaped(t *testing.T) {
	var output strings.Builder
	err := renderFullReport(&output, webRun{ID: "test"}, webAnalytics{}, func(visit func(reportRecord) error) error {
		return visit(reportRecord{"target_id": int64(1), "safe_url": "https://example.com/", "title": "<script>alert(1)</script>", "h1": "Complete heading"})
	})
	if err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{"<details><summary>Повні метрики</summary><dl>", "&lt;script&gt;alert(1)&lt;/script&gt;", "Complete heading", "id=\"target-1\""} {
		if !strings.Contains(html, want) {
			t.Fatalf("report omitted %q", want)
		}
	}
	if strings.Contains(html, "<script") || strings.Contains(html, "<details open") {
		t.Fatal("report executed or eagerly displayed untrusted metrics")
	}
}
