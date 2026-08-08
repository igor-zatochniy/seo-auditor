package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reportsDirectory     = "reports"
	latestReportFilename = "latest-report.html"
)

type auditReportPaths struct {
	Latest  string
	Archive string
}

type auditReportSummary struct {
	RunID          string
	Status         string
	StatusTone     string
	TotalURLs      int64
	SuccessfulURLs int64
	FailedURLs     int64
	StartedAt      string
	FinishedAt     string
	GeneratedAt    string
}

type auditReportRow struct {
	URL              string
	HTTPCode         string
	Status           string
	StatusTone       string
	Title            string
	Description      string
	H1               string
	Links            string
	ImagesMissingAlt int
	Robots           string
	WordCount        int
	Duration         string
	Error            string
}

type reportRowIterator func() (auditReportRow, bool, error)

var (
	reportHeaderTemplate = template.Must(template.New("report-header").Parse(reportHeaderHTML))
	reportRowTemplate    = template.Must(template.New("report-row").Parse(reportRowHTML))
	reportFooterTemplate = template.Must(template.New("report-footer").Parse(reportFooterHTML))
)

func publishAuditReport(dbPool *pgxpool.Pool, cfg Config) {
	timeout := cfg.ReportExportTimeout
	if timeout <= 0 {
		timeout = DefaultReportExportTimeout
	}
	reportCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	paths, err := exportAuditReport(reportCtx, dbPool, cfg.RunID, reportsDirectory, time.Now())
	if err != nil {
		slog.Error("Не вдалося експортувати HTML-звіт аудиту", "error", err)
		return
	}
	slog.Info(
		"HTML-звіт аудиту створено",
		"latest_report", paths.Latest,
		"archive_report", paths.Archive,
	)

	if err := openReportInBrowser(paths.Latest); err != nil {
		slog.Warn("Не вдалося відкрити HTML-звіт у системному браузері", "report", paths.Latest, "error", err)
	}
}

func exportAuditReport(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	reportDir string,
	generatedAt time.Time,
) (auditReportPaths, error) {
	summary, err := loadAuditReportSummary(ctx, dbPool, runID, generatedAt)
	if err != nil {
		return auditReportPaths{}, err
	}

	rows, err := dbPool.Query(
		ctx,
		`SELECT safe_url,
		        status_code,
		        scan_status,
		        COALESCE(title, ''),
		        COALESCE(description, ''),
		        COALESCE(h1, ''),
		        COALESCE(internal_links_count, 0),
		        COALESCE(external_links_count, 0),
		        COALESCE(links_count, 0),
		        COALESCE(images_missing_alt, 0),
		        COALESCE(meta_robots, ''),
		        COALESCE(x_robots_tag, ''),
		        COALESCE(robots_allowed, FALSE),
		        COALESCE(robots_outcome, 'not_checked'),
		        COALESCE(word_count, 0),
		        COALESCE(duration_ms, 0),
		        COALESCE(error_code, ''),
		        COALESCE(error_message, '')
		 FROM audit_results
		 WHERE run_id = $1
		 ORDER BY target_id`,
		runID,
	)
	if err != nil {
		return auditReportPaths{}, fmt.Errorf("query audit report results for run %s: %w", runID, err)
	}
	defer rows.Close()

	next := func() (auditReportRow, bool, error) {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return auditReportRow{}, false, err
			}
			return auditReportRow{}, false, nil
		}

		var (
			safeURL          string
			statusCode       sql.NullInt32
			scanStatus       string
			title            string
			description      string
			h1               string
			internalLinks    int
			externalLinks    int
			totalLinks       int
			imagesMissingAlt int
			metaRobots       string
			xRobotsTag       string
			robotsAllowed    bool
			robotsOutcome    string
			wordCount        int
			durationMS       int64
			errorCode        string
			errorMessage     string
		)
		if err := rows.Scan(
			&safeURL,
			&statusCode,
			&scanStatus,
			&title,
			&description,
			&h1,
			&internalLinks,
			&externalLinks,
			&totalLinks,
			&imagesMissingAlt,
			&metaRobots,
			&xRobotsTag,
			&robotsAllowed,
			&robotsOutcome,
			&wordCount,
			&durationMS,
			&errorCode,
			&errorMessage,
		); err != nil {
			return auditReportRow{}, false, fmt.Errorf("scan audit report result: %w", err)
		}

		return newAuditReportRow(
			safeURL,
			statusCode,
			scanStatus,
			title,
			description,
			h1,
			internalLinks,
			externalLinks,
			totalLinks,
			imagesMissingAlt,
			metaRobots,
			xRobotsTag,
			robotsAllowed,
			robotsOutcome,
			wordCount,
			durationMS,
			errorCode,
			errorMessage,
		), true, nil
	}

	paths, err := writeAuditReportFiles(reportDir, summary, next, generatedAt)
	if err != nil {
		return auditReportPaths{}, fmt.Errorf("write audit report for run %s: %w", runID, err)
	}
	return paths, nil
}

func loadAuditReportSummary(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	generatedAt time.Time,
) (auditReportSummary, error) {
	var (
		status         string
		totalURLs      int64
		successfulURLs int64
		failedURLs     int64
		startedAt      time.Time
		finishedAt     sql.NullTime
	)
	if err := dbPool.QueryRow(
		ctx,
		`SELECT status, total_urls, successful_urls, failed_urls, started_at, finished_at
		 FROM audit_runs
		 WHERE id = $1`,
		runID,
	).Scan(&status, &totalURLs, &successfulURLs, &failedURLs, &startedAt, &finishedAt); err != nil {
		return auditReportSummary{}, fmt.Errorf("query audit report summary for run %s: %w", runID, err)
	}

	finishedAtText := "-"
	if finishedAt.Valid {
		finishedAtText = formatReportTime(finishedAt.Time)
	}
	return auditReportSummary{
		RunID:          runID,
		Status:         reportStatusLabel(status),
		StatusTone:     reportStatusTone(status, sql.NullInt32{}, ""),
		TotalURLs:      totalURLs,
		SuccessfulURLs: successfulURLs,
		FailedURLs:     failedURLs,
		StartedAt:      formatReportTime(startedAt),
		FinishedAt:     finishedAtText,
		GeneratedAt:    formatReportTime(generatedAt),
	}, nil
}

func newAuditReportRow(
	safeURL string,
	statusCode sql.NullInt32,
	scanStatus string,
	title string,
	description string,
	h1 string,
	internalLinks int,
	externalLinks int,
	totalLinks int,
	imagesMissingAlt int,
	metaRobots string,
	xRobotsTag string,
	robotsAllowed bool,
	robotsOutcome string,
	wordCount int,
	durationMS int64,
	errorCode string,
	errorMessage string,
) auditReportRow {
	httpCode := "-"
	if statusCode.Valid {
		httpCode = strconv.FormatInt(int64(statusCode.Int32), 10)
	}

	accessLabel := "заборонено"
	if robotsAllowed {
		accessLabel = "дозволено"
	}
	robotsParts := []string{
		fmt.Sprintf("Результат: %s", reportRobotsOutcomeLabel(robotsOutcome)),
		"Доступ: " + accessLabel,
	}
	if strings.TrimSpace(metaRobots) != "" {
		robotsParts = append(robotsParts, "Meta: "+metaRobots)
	}
	if strings.TrimSpace(xRobotsTag) != "" {
		robotsParts = append(robotsParts, "X-Robots-Tag: "+xRobotsTag)
	}

	errorParts := make([]string, 0, 2)
	if strings.TrimSpace(errorCode) != "" {
		errorParts = append(errorParts, strings.TrimSpace(errorCode))
	}
	if strings.TrimSpace(errorMessage) != "" {
		errorParts = append(errorParts, strings.TrimSpace(errorMessage))
	}
	errorText := "-"
	if len(errorParts) > 0 {
		errorText = strings.Join(errorParts, ": ")
	}

	return auditReportRow{
		URL:              displayReportValue(safeURL),
		HTTPCode:         httpCode,
		Status:           reportStatusLabel(scanStatus),
		StatusTone:       reportStatusTone(scanStatus, statusCode, errorCode),
		Title:            displayReportValue(title),
		Description:      displayReportValue(description),
		H1:               displayReportValue(h1),
		Links:            fmt.Sprintf("Внутрішні: %d\nЗовнішні: %d\nУсього: %d", internalLinks, externalLinks, totalLinks),
		ImagesMissingAlt: imagesMissingAlt,
		Robots:           strings.Join(robotsParts, "\n"),
		WordCount:        wordCount,
		Duration:         formatReportDuration(durationMS),
		Error:            errorText,
	}
}

func writeAuditReportFiles(
	reportDir string,
	summary auditReportSummary,
	next reportRowIterator,
	generatedAt time.Time,
) (auditReportPaths, error) {
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return auditReportPaths{}, fmt.Errorf("create report directory: %w", err)
	}

	archiveName := fmt.Sprintf(
		"seo-audit-%s-%s.html",
		generatedAt.Format("2006-01-02_15-04-05"),
		reportRunSuffix(summary.RunID),
	)
	archivePath := filepath.Join(reportDir, archiveName)
	if err := writeFileAtomically(archivePath, func(writer io.Writer) error {
		return renderAuditReport(writer, summary, next)
	}); err != nil {
		return auditReportPaths{}, fmt.Errorf("write archive report: %w", err)
	}

	latestPath := filepath.Join(reportDir, latestReportFilename)
	if err := copyFileAtomically(archivePath, latestPath); err != nil {
		return auditReportPaths{}, fmt.Errorf("write latest report: %w", err)
	}

	return auditReportPaths{Latest: latestPath, Archive: archivePath}, nil
}

func renderAuditReport(writer io.Writer, summary auditReportSummary, next reportRowIterator) error {
	buffered := bufio.NewWriterSize(writer, 64*1024)
	if err := reportHeaderTemplate.Execute(buffered, summary); err != nil {
		return fmt.Errorf("render report header: %w", err)
	}
	for {
		row, ok, err := next()
		if err != nil {
			return fmt.Errorf("read report row: %w", err)
		}
		if !ok {
			break
		}
		if err := reportRowTemplate.Execute(buffered, row); err != nil {
			return fmt.Errorf("render report row: %w", err)
		}
	}
	if err := reportFooterTemplate.Execute(buffered, nil); err != nil {
		return fmt.Errorf("render report footer: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("flush report output: %w", err)
	}
	return nil
}

func writeFileAtomically(path string, write func(io.Writer) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := write(temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(temporaryPath, path)
}

func copyFileAtomically(sourcePath, targetPath string) error {
	return writeFileAtomically(targetPath, func(writer io.Writer) error {
		source, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer source.Close()
		_, err = io.Copy(writer, source)
		return err
	})
}

func replaceFile(sourcePath, targetPath string) error {
	if err := os.Rename(sourcePath, targetPath); err == nil {
		return nil
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(sourcePath, targetPath)
}

func openReportInBrowser(path string) error {
	return openReportInBrowserForOS(runtime.GOOS, path, startReportBrowserCommand)
}

func openReportInBrowserForOS(goos, path string, start func(string, ...string) error) error {
	if goos != "windows" {
		return nil
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve report path: %w", err)
	}
	reportURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath)}).String()
	if err := start("rundll32.exe", "url.dll,FileProtocolHandler", reportURL); err != nil {
		return fmt.Errorf("start default browser: %w", err)
	}
	return nil
}

func startReportBrowserCommand(name string, arguments ...string) error {
	return exec.Command(name, arguments...).Start()
}

func reportStatusTone(status string, statusCode sql.NullInt32, errorCode string) string {
	if strings.TrimSpace(errorCode) != "" || status == scanStatusFailed {
		return "tone-danger"
	}
	if status == auditRunStatusCompletedWithErrors || status == auditRunStatusCanceled ||
		status == scanStatusRedirect || status == scanStatusBlockedByRobots {
		return "tone-warning"
	}
	if statusCode.Valid && (statusCode.Int32 < 200 || statusCode.Int32 >= 300) {
		return "tone-warning"
	}
	if status == auditRunStatusCompleted || status == scanStatusCompleted {
		return "tone-success"
	}
	return "tone-neutral"
}

func reportStatusLabel(status string) string {
	switch status {
	case auditRunStatusCompleted:
		return "Завершено"
	case auditRunStatusCompletedWithErrors:
		return "Завершено з помилками"
	case auditRunStatusFailed:
		return "Помилка"
	case auditRunStatusCanceled:
		return "Скасовано"
	case auditRunStatusAbandoned:
		return "Перервано"
	case scanStatusRedirect:
		return "Редирект"
	case scanStatusBlockedByRobots:
		return "Заблоковано robots.txt"
	default:
		return displayReportValue(status)
	}
}

func reportRobotsOutcomeLabel(outcome string) string {
	switch outcome {
	case robotsOutcomeAllowed:
		return "дозволено"
	case robotsOutcomeDisallowed:
		return "заборонено"
	case robotsOutcomeUnavailable:
		return "недоступний"
	case robotsOutcomeNotChecked:
		return "не перевірено"
	default:
		return displayReportValue(outcome)
	}
}

func displayReportValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func formatReportTime(value time.Time) string {
	return value.Format("02.01.2006 15:04:05 MST")
}

func formatReportDuration(milliseconds int64) string {
	if milliseconds < 1000 {
		return fmt.Sprintf("%d ms", milliseconds)
	}
	return fmt.Sprintf("%.2f s", float64(milliseconds)/1000)
}

func reportRunSuffix(runID string) string {
	compact := strings.ReplaceAll(runID, "-", "")
	if len(compact) > 8 {
		return compact[:8]
	}
	if compact == "" {
		return "run"
	}
	return compact
}
