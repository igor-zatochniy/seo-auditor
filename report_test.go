package main

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderAuditReportEscapesUntrustedValues(t *testing.T) {
	summary := auditReportSummary{
		RunID:          `run-<script>alert("summary")</script>`,
		Status:         "Завершено",
		StatusTone:     "tone-success",
		TotalURLs:      1,
		SuccessfulURLs: 1,
		StartedAt:      "05.08.2026 10:00:00 EEST",
		FinishedAt:     "05.08.2026 10:00:01 EEST",
		GeneratedAt:    "05.08.2026 10:00:02 EEST",
	}
	rows := []auditReportRow{{
		URL:              `https://example.com/?q=<script>alert("url")</script>`,
		HTTPCode:         "200",
		Status:           "Завершено",
		StatusTone:       "tone-success",
		Title:            `<img src=x onerror="alert('title')">`,
		Description:      `" onmouseover="alert('description')`,
		H1:               "Перевірений H1",
		Links:            "Внутрішні: 2\nЗовнішні: 1\nУсього: 3",
		ImagesMissingAlt: 1,
		Robots:           "Результат: allowed",
		WordCount:        42,
		Duration:         "125 ms",
		Error:            `<script>alert("error")</script>`,
	}}
	index := 0
	next := func() (auditReportRow, bool, error) {
		if index >= len(rows) {
			return auditReportRow{}, false, nil
		}
		row := rows[index]
		index++
		return row, true, nil
	}

	var output bytes.Buffer
	if err := renderAuditReport(&output, summary, next); err != nil {
		t.Fatalf("renderAuditReport returned error: %v", err)
	}
	html := output.String()
	if strings.Contains(html, "<script>alert") || strings.Contains(html, "<img src=x") {
		t.Fatalf("untrusted HTML was not escaped: %s", html)
	}
	for _, escaped := range []string{`&lt;script&gt;alert`, `&lt;img src=x onerror=`, `&#34; onmouseover=&#34;`} {
		if !strings.Contains(html, escaped) {
			t.Fatalf("escaped value %q is missing from report", escaped)
		}
	}
}

func TestWriteAuditReportFilesCreatesLatestAndArchive(t *testing.T) {
	reportDir := t.TempDir()
	generatedAt := time.Date(2026, time.August, 5, 14, 35, 20, 0, time.FixedZone("EEST", 3*60*60))
	summary := auditReportSummary{
		RunID:       "9d532d38-2142-4f5a-9b68-6351ef5ed18c",
		Status:      "Завершено",
		StatusTone:  "tone-success",
		TotalURLs:   1,
		StartedAt:   formatReportTime(generatedAt.Add(-time.Second)),
		FinishedAt:  formatReportTime(generatedAt),
		GeneratedAt: formatReportTime(generatedAt),
	}
	returned := false
	next := func() (auditReportRow, bool, error) {
		if returned {
			return auditReportRow{}, false, nil
		}
		returned = true
		return auditReportRow{
			URL:        "https://example.com",
			HTTPCode:   "200",
			Status:     "Завершено",
			StatusTone: "tone-success",
		}, true, nil
	}

	paths, err := writeAuditReportFiles(reportDir, summary, next, generatedAt)
	if err != nil {
		t.Fatalf("writeAuditReportFiles returned error: %v", err)
	}
	if filepath.Base(paths.Latest) != latestReportFilename {
		t.Fatalf("latest report path = %q", paths.Latest)
	}
	if filepath.Base(paths.Archive) != "seo-audit-2026-08-05_14-35-20-9d532d38.html" {
		t.Fatalf("archive report path = %q", paths.Archive)
	}

	latest, err := os.ReadFile(paths.Latest)
	if err != nil {
		t.Fatalf("read latest report: %v", err)
	}
	archive, err := os.ReadFile(paths.Archive)
	if err != nil {
		t.Fatalf("read archive report: %v", err)
	}
	if !bytes.Equal(latest, archive) {
		t.Fatal("latest report differs from archive")
	}
	if !bytes.Contains(latest, []byte("https://example.com")) {
		t.Fatal("report result is missing from generated files")
	}
}

func TestWriteAuditReportFilesKeepsLatestWhenRenderingFails(t *testing.T) {
	reportDir := t.TempDir()
	latestPath := filepath.Join(reportDir, latestReportFilename)
	const previousReport = "previous complete report"
	if err := os.WriteFile(latestPath, []byte(previousReport), 0o644); err != nil {
		t.Fatalf("write previous latest report: %v", err)
	}

	expectedErr := errors.New("result stream interrupted")
	next := func() (auditReportRow, bool, error) {
		return auditReportRow{}, false, expectedErr
	}
	_, err := writeAuditReportFiles(
		reportDir,
		auditReportSummary{RunID: "9d532d38-2142-4f5a-9b68-6351ef5ed18c"},
		next,
		time.Date(2026, time.August, 5, 14, 35, 20, 0, time.UTC),
	)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("writeAuditReportFiles error = %v", err)
	}

	latest, err := os.ReadFile(latestPath)
	if err != nil {
		t.Fatalf("read preserved latest report: %v", err)
	}
	if string(latest) != previousReport {
		t.Fatalf("latest report changed after failed export: %q", latest)
	}
	archives, err := filepath.Glob(filepath.Join(reportDir, "seo-audit-*.html"))
	if err != nil {
		t.Fatalf("list archive reports: %v", err)
	}
	if len(archives) != 0 {
		t.Fatalf("failed export left an archive behind: %#v", archives)
	}
}

func TestOpenReportInBrowserForOSUsesWindowsFileHandler(t *testing.T) {
	var command string
	var arguments []string
	start := func(name string, args ...string) error {
		command = name
		arguments = append([]string(nil), args...)
		return nil
	}

	if err := openReportInBrowserForOS("windows", "reports/latest-report.html", start); err != nil {
		t.Fatalf("openReportInBrowserForOS returned error: %v", err)
	}
	if command != "rundll32.exe" {
		t.Fatalf("browser command = %q", command)
	}
	if len(arguments) != 2 || arguments[0] != "url.dll,FileProtocolHandler" || !strings.HasPrefix(arguments[1], "file:") {
		t.Fatalf("browser arguments = %#v", arguments)
	}

	command = ""
	arguments = nil
	if err := openReportInBrowserForOS("linux", "reports/latest-report.html", start); err != nil {
		t.Fatalf("non-Windows open returned error: %v", err)
	}
	if command != "" || arguments != nil {
		t.Fatalf("non-Windows browser command was started: %q %#v", command, arguments)
	}
}

func TestNewAuditReportRowBuildsVisibleStatusFields(t *testing.T) {
	row := newAuditReportRow(
		"https://example.com/page",
		sql.NullInt32{Int32: 503, Valid: true},
		scanStatusCompleted,
		"Title",
		"Description",
		"H1",
		2,
		3,
		5,
		1,
		"noindex",
		"nofollow",
		true,
		robotsOutcomeAllowed,
		120,
		1250,
		"",
		"",
	)

	if row.HTTPCode != "503" || row.StatusTone != "tone-warning" || row.Duration != "1.25 s" {
		t.Fatalf("unexpected report row status: %#v", row)
	}
	if !strings.Contains(row.Links, "Усього: 5") ||
		!strings.Contains(row.Robots, "Результат: дозволено") ||
		!strings.Contains(row.Robots, "X-Robots-Tag: nofollow") {
		t.Fatalf("unexpected report row details: %#v", row)
	}
}
