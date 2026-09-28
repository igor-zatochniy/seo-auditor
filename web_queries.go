package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const parsedPagePredicate = "r.scan_status = 'completed' AND r.status_code = 200"
const noindexPredicate = "(COALESCE(r.meta_robots, '') || ',' || COALESCE(r.x_robots_tag, '')) ~* '(^|[[:space:],:;])(noindex|none)($|[[:space:],;])'"

var resultFilters = map[string]string{
	"all": "TRUE", "errors": "r.scan_status = 'failed'", "4xx": "r.status_code BETWEEN 400 AND 499",
	"5xx": "r.status_code BETWEEN 500 AND 599", "redirects": "r.scan_status = 'redirect'",
	"robots_blocked": "r.scan_status = 'blocked_by_robots'",
	"title_missing":  "r.title_status = 'Missing'", "title_short": "r.title_status = 'Too Short'", "title_long": "r.title_status = 'Too Long'",
	"description_missing": "r.description_status = 'Missing'", "description_short": "r.description_status = 'Too Short'", "description_long": "r.description_status = 'Too Long'",
	"title_recommended":        parsedPagePredicate + " AND r.title_status='Recommended'",
	"title_borderline":         parsedPagePredicate + " AND r.title_status='Borderline'",
	"title_high_risk":          parsedPagePredicate + " AND r.title_status='High truncation risk'",
	"description_mobile_risk":  parsedPagePredicate + " AND r.description_mobile_status IN ('May truncate','Likely truncate')",
	"description_desktop_risk": parsedPagePredicate + " AND r.description_status='May truncate'",
	"h1_missing":               parsedPagePredicate + " AND r.h1_count = 0", "h1_multiple": parsedPagePredicate + " AND r.h1_count > 1",
	"canonical_missing": parsedPagePredicate + " AND COALESCE(r.canonical_url,'') = ''",
	"canonical_other":   parsedPagePredicate + " AND COALESCE(r.canonical_url,'') <> '' AND NOT r.is_self_canonical",
	"noindex":           noindexPredicate, "images_alt": parsedPagePredicate + " AND r.images_missing_alt > 0",
	"json_ld_absent": parsedPagePredicate + " AND NOT r.has_json_ld", "viewport_absent": parsedPagePredicate + " AND NOT r.has_viewport",
}

func init() {
	var parts []string
	for _, field := range truncationFields {
		parts = append(parts, "r."+field+"_truncated")
	}
	resultFilters["truncated"] = "(" + strings.Join(parts, " OR ") + ")"
}

type resultQuery struct {
	After          int64
	Limit          int
	Search, Filter string
}

func parseResultQuery(v url.Values) (resultQuery, error) {
	q := resultQuery{After: math.MinInt64, Limit: 50, Filter: v.Get("filter"), Search: strings.TrimSpace(v.Get("search"))}
	if q.Filter == "" {
		q.Filter = "all"
	}
	if _, ok := resultFilters[q.Filter]; !ok {
		return q, fmt.Errorf("невідомий фільтр")
	}
	if len(q.Search) > 512 {
		return q, fmt.Errorf("пошук перевищує 512 байтів")
	}
	if v.Get("sort") != "" && v.Get("sort") != "target_id" {
		return q, fmt.Errorf("дозволено сортування лише за target_id")
	}
	var err error
	if v.Get("after") != "" {
		q.After, err = strconv.ParseInt(v.Get("after"), 10, 64)
		if err != nil {
			return q, fmt.Errorf("некоректний cursor")
		}
	}
	if v.Get("limit") != "" {
		q.Limit, err = strconv.Atoi(v.Get("limit"))
		if err != nil || q.Limit < 1 || q.Limit > 200 {
			return q, fmt.Errorf("розмір сторінки: 1–200")
		}
	}
	return q, nil
}

func reportSelectSQL() string {
	columns := make([]string, 0, len(reportFields))
	for _, field := range reportFields {
		if field.Key == "title_status" || field.Key == "description_status" {
			columns = append(columns, "CASE WHEN r.serp_width_model='' AND r."+field.Key+" IN ('OK','Too Short','Too Long') THEN 'Legacy: ' || r."+field.Key+" ELSE r."+field.Key+" END AS "+field.Key)
			continue
		}
		prefix := "r."
		if field.Key == "attempts" || field.Key == "started_at" || field.Key == "finished_at" {
			prefix = "t."
		}
		columns = append(columns, prefix+field.Key)
	}
	return "SELECT " + strings.Join(columns, ",") + " FROM audit_results r JOIN audit_run_targets t USING (run_id, target_id)"
}

func scanReportRecord(rows pgx.Rows) (reportRecord, error) {
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	var record reportRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	return sanitizeReportRecord(record), nil
}

// Defense in depth for imported data, including URLs embedded in metadata.
func sanitizeReportRecord(record reportRecord) reportRecord {
	for key, value := range record {
		if text, ok := value.(string); ok {
			switch key {
			case "safe_url", "redirect_url", "canonical_url", "og_image":
				record[key] = redactURL(text)
			default:
				record[key] = redactText(text)
			}
		}
	}
	return record
}

type resultPage struct {
	Rows []reportRecord `json:"rows"`
	Next string         `json:"next"`
}

func loadResultPage(ctx context.Context, pool *pgxpool.Pool, id string, q resultQuery) (resultPage, error) {
	page := resultPage{Rows: make([]reportRecord, 0, q.Limit)}
	predicate, ok := resultFilters[q.Filter]
	if !ok {
		return page, fmt.Errorf("unknown filter")
	}
	rows, err := pool.Query(ctx, "SELECT row_to_json(p) FROM ("+reportSelectSQL()+
		" WHERE r.run_id=$1 AND r.target_id > $2 AND ($3='' OR position(lower($3) in lower(r.safe_url)) > 0) AND ("+predicate+") ORDER BY r.target_id LIMIT $4) p",
		id, q.After, q.Search, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Rows) == q.Limit {
			page.Next = fmt.Sprint(page.Rows[len(page.Rows)-1]["target_id"])
			break
		}
		record, err := scanReportRecord(rows)
		if err != nil {
			return page, err
		}
		page.Rows = append(page.Rows, record)
	}
	return page, rows.Err()
}

const webRunColumns = `run.id::TEXT, run.status, run.total_urls, run.successful_urls, run.failed_urls,
	run.started_at, run.finished_at,
	(run.targets_captured_at IS NOT NULL AND (run.status IN ('failed','abandoned') OR
	 (run.status='running' AND run.heartbeat_at < CURRENT_TIMESTAMP - $2::INTERVAL)))`

func scanWebRun(row pgx.Row) (webRun, error) {
	var run webRun
	err := row.Scan(&run.ID, &run.Status, &run.Total, &run.Successful, &run.Failed, &run.StartedAt, &run.FinishedAt, &run.Resumable)
	return run, err
}
func loadWebRun(ctx context.Context, pool *pgxpool.Pool, cfg Config, id string) (webRun, error) {
	return scanWebRun(pool.QueryRow(ctx, "SELECT "+webRunColumns+" FROM audit_runs run WHERE run.id=$1", id, effectiveStaleRunThreshold(cfg).String()))
}

type historyCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func decodeHistoryCursor(s string) (historyCursor, error) {
	var c historyCursor
	if len(s) > 256 {
		return c, fmt.Errorf("invalid cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil || !runIDPattern.MatchString(c.ID) || c.At.IsZero() {
		return c, fmt.Errorf("invalid cursor")
	}
	return c, nil
}

type historyPage struct {
	Runs []webRun `json:"runs"`
	Next string   `json:"next"`
}

func loadRunHistory(ctx context.Context, pool *pgxpool.Pool, cfg Config, cursor string) (historyPage, error) {
	page := historyPage{Runs: []webRun{}}
	before := historyCursor{At: time.Now().Add(24 * time.Hour), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	var err error
	if cursor != "" {
		before, err = decodeHistoryCursor(cursor)
		if err != nil {
			return page, err
		}
	}
	rows, err := pool.Query(ctx, "SELECT "+webRunColumns+" FROM audit_runs run WHERE (run.started_at,run.id)<($1,$3::UUID) ORDER BY run.started_at DESC,run.id DESC LIMIT 51", before.At, effectiveStaleRunThreshold(cfg).String(), before.ID)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Runs) == 50 {
			last := page.Runs[49]
			b, _ := json.Marshal(historyCursor{last.StartedAt, last.ID})
			page.Next = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		run, err := scanWebRun(rows)
		if err != nil {
			return page, err
		}
		page.Runs = append(page.Runs, run)
	}
	return page, rows.Err()
}

func loadWebProgress(ctx context.Context, pool *pgxpool.Pool, cfg Config, id string) (webProgress, error) {
	p := webProgress{Counts: map[string]int64{"pending": 0, "running": 0, "completed": 0, "failed": 0, "canceled": 0, "abandoned": 0}}
	var err error
	p.Run, err = loadWebRun(ctx, pool, cfg, id)
	if err != nil {
		return p, err
	}
	rows, err := pool.Query(ctx, "SELECT status,COUNT(*) FROM audit_run_targets WHERE run_id=$1 GROUP BY status", id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return p, err
		}
		p.Counts[status] = n
	}
	if p.Run.Total > 0 {
		p.Percentage = 100 * float64(p.Counts["completed"]+p.Counts["failed"]+p.Counts["canceled"]) / float64(p.Run.Total)
	}
	return p, rows.Err()
}
