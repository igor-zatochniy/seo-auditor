package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

func iterateReportRecords(ctx context.Context, pool reportQuerier, id string, visit func(reportRecord) error) error {
	rows, err := pool.Query(ctx, "SELECT row_to_json(p) FROM ("+reportSelectSQL()+" WHERE r.run_id=$1 ORDER BY r.target_id) p", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanReportRecord(rows)
		if err != nil {
			return err
		}
		if err = visit(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func reportValue(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func safeCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\ufeff' })
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") ||
		(len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0]))) {
		return "'" + value
	}
	return value
}

func writeCSVReport(w io.Writer, visit func(func(reportRecord) error) error) error {
	writer := csv.NewWriter(w)
	header := make([]string, len(reportFields))
	for i, f := range reportFields {
		header[i] = f.Key
	}
	if err := writer.Write(header); err != nil {
		return err
	}
	if err := visit(func(record reportRecord) error {
		cells := make([]string, len(reportFields))
		for i, f := range reportFields {
			cells[i] = safeCSVCell(reportValue(record[f.Key]))
		}
		return writer.Write(cells)
	}); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

type reportDetail struct{ Label, Value, Group string }

func recordDetails(record reportRecord) []reportDetail {
	fields := make([]reportDetail, 0, len(reportFields))
	for _, f := range reportFields {
		fields = append(fields, reportDetail{f.Label, reportValue(record[f.Key]), f.Group})
	}
	return fields
}

var extendedHeader = template.Must(template.New("full-header").Parse(`<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>SEO Auditor · {{.Run.ID}}</title><style>
*{box-sizing:border-box}body{margin:0;background:#f6f8fa;color:#20262d;font:14px/1.5 system-ui,sans-serif;letter-spacing:0;overflow-wrap:anywhere}main{max-width:1200px;margin:32px auto;padding:0 24px}h1{font-size:28px}h2{font-size:20px}h3{font-size:15px}small{color:#53636a}.summary,.charts{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:20px}.summary{padding:20px 0;border-block:1px solid #ccd8d5}.chart{border-bottom:1px solid #ccd8d5;padding-bottom:16px}.bar{display:grid;grid-template-columns:1fr auto;gap:8px;margin:8px 0}progress{width:100%;height:10px;accent-color:#087f68}.result{margin:32px 0;border-top:2px solid #087f68;padding-top:12px;break-inside:avoid}dl{display:grid;grid-template-columns:minmax(150px,1fr) 3fr;margin:0}dt,dd{padding:7px;border-bottom:1px solid #e0e5e7;overflow-wrap:anywhere;white-space:pre-wrap}dd{margin:0}dt{color:#52646d}.status{font-weight:700;color:#087f68}.error{color:#a43038}@media(max-width:600px){main{padding:0 14px}dl{grid-template-columns:1fr}dt{border:0;padding-bottom:0}}
</style></head><body><main><h1>SEO Auditor</h1><p>{{.Run.ID}}</p><section class="summary"><div>Статус: <strong>{{.Run.Status}}</strong></div><div>URL: <strong>{{.Run.Total}}</strong></div><div>Успішно: {{.Run.Successful}}</div><div>Помилки: {{.Run.Failed}}</div><div>Початок: {{.Run.StartedAt}}</div><div>Завершення: {{.Run.FinishedAt}}</div></section><h2>Технічні сигнали</h2><p>HTML-метрики: {{.Analytics.Parsed}} розібраних сторінок. SERP width: Title ≤580 px Recommended, 581–600 px Borderline, >600 px High truncation risk. Description ≤680 px Safe desktop/mobile; 681–920 px Safe desktop / May truncate mobile; >920 px May truncate desktop / Likely truncate mobile. Liberation Sans 20 px / 14 px: оцінка, не гарантія відображення пошуковиком. Legacy позначає попередні character-based результати. Noindex враховує будь-який scope, а не остаточну індексованість.</p><section class="charts">{{range .Analytics.Groups}}{{$group := .}}<div class="chart"><h3>{{.Label}}</h3>{{range .Buckets}}<div class="bar"><span>{{.Label}}</span><strong>{{printf "%.1f" .Value}}</strong></div><progress max="{{$group.Maximum}}" value="{{.Value}}" aria-label="{{.Label}}"></progress>{{end}}</div>{{end}}</section><h2>Повні результати</h2>`))
var extendedRow = template.Must(template.New("full-row").Parse(`<article class="result"><h3>{{.safe_url}}</h3><p class="status">{{.scan_status}} · HTTP {{.status_code}}</p><dl>{{range .details}}<dt>{{.Group}} / {{.Label}}</dt><dd>{{.Value}}</dd>{{end}}</dl></article>`))

func renderFullReport(w io.Writer, run webRun, analytics webAnalytics, visit func(func(reportRecord) error) error) error {
	buffer := bufio.NewWriterSize(w, 64<<10)
	if err := extendedHeader.Execute(buffer, struct {
		Run       webRun
		Analytics webAnalytics
	}{run, analytics}); err != nil {
		return err
	}
	if err := visit(func(record reportRecord) error {
		return extendedRow.Execute(buffer, map[string]any{"safe_url": record["safe_url"], "scan_status": record["scan_status"], "status_code": record["status_code"], "details": recordDetails(record)})
	}); err != nil {
		return err
	}
	if _, err := io.WriteString(buffer, "</main></body></html>"); err != nil {
		return err
	}
	return buffer.Flush()
}

func (s *webServer) export(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	format := r.PathValue("format")
	if format != "html" && format != "csv" {
		writeWebError(w, 400, "invalid_format", "Формат: html або csv")
		return
	}
	// A repeatable-read transaction produces a consistent export, including during a resumed run.
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), effectiveDBWriteTimeout(s.cfg))
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// Use the same connection for the summary, analytics and stream.
	run, err := scanWebRun(tx.QueryRow(r.Context(), "SELECT "+webRunColumns+" FROM audit_runs run WHERE run.id=$1", id, effectiveStaleRunThreshold(s.cfg).String()))
	if err != nil {
		webDBError(w, err)
		return
	}
	if run.Status == auditRunStatusRunning {
		writeWebError(w, 409, "audit_running", "Повний експорт доступний після завершення аудиту")
		return
	}
	visit := func(fn func(reportRecord) error) error {
		return iterateReportRecords(r.Context(), tx, id, fn)
	}
	var analytics webAnalytics
	if format == "html" {
		analytics, err = loadWebAnalytics(r.Context(), tx, id)
		if err != nil {
			webDBError(w, err)
			return
		}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="seo-audit-`+id+`.`+format+`"`)
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		err = writeCSVReport(w, visit)
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; sandbox")
		err = renderFullReport(w, run, analytics, visit)
	}
	if err != nil {
		slog.Error("Не вдалося завершити експорт", "run_id", id, "error", sanitizeError(err))
		panic(http.ErrAbortHandler)
	}
}
