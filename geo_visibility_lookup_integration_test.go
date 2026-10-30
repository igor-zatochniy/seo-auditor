//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

type visibilityQueryTrace struct {
	mu             sync.Mutex
	queries        []string
	cancelOnCorpus context.CancelFunc
}

func (trace *visibilityQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.mu.Lock()
	trace.queries = append(trace.queries, strings.ToLower(data.SQL))
	if strings.Contains(strings.ToLower(data.SQL), "from audit_results") && trace.cancelOnCorpus != nil {
		trace.cancelOnCorpus()
		trace.cancelOnCorpus = nil
	}
	trace.mu.Unlock()
	return ctx
}

func TestGEOVisibilityCanceledLookupRollsBackAndCanRetry(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Tracer = &visibilityQueryTrace{cancelOnCorpus: cancel}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	app.pool = pool
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_ai", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", CSV: "Page,Impressions\nhttps://example.com/report?token=alpha,100\n"}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.storeVisibilityImport(ctx, report.ID, input, records); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup: %v", err)
	}
	var imports, results int
	if err := pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_visibility_imports WHERE report_id=$1", report.ID).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&results); err != nil || imports != 0 || results != 2 {
		t.Fatalf("rollback changed data: imports=%d results=%d err=%v", imports, results, err)
	}
	records, _ = geo.ParseVisibilityCSV(&input)
	if _, err := app.storeVisibilityImport(t.Context(), report.ID, input, records); err != nil || records[0].URLMatch != "exact" {
		t.Fatalf("retry after cancellation: %+v err=%v", records, err)
	}
}

func (*visibilityQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestGEOVisibilityQueryOnlyDoesNotReadSourceCorpus(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	trace := &visibilityQueryTrace{}
	poolConfig.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	app.pool = pool
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_web", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", CSV: "Query,Clicks,Impressions\nchargeback protection,1,100\n"}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.storeVisibilityImport(t.Context(), report.ID, input, records); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	for _, statement := range trace.queries {
		if strings.Contains(statement, "audit_results") || strings.Contains(statement, "visibility_origins") {
			t.Fatalf("query-only import read source URLs: %s", statement)
		}
	}
	if records[0].TargetID != nil || records[0].ReportOrdinal == nil {
		t.Fatalf("query-only binding changed: %+v", records[0])
	}
}

func insertVisibilityLookupTarget(t *testing.T, app *webServer, cfg Config, id int64, raw, keyID string, canonical *crawler.URLReference) {
	t.Helper()
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,finished_at,request_url_cleared_at)
 VALUES($1,$2,'','completed',NOW(),NOW())`, cfg.RunID, id); err != nil {
		t.Fatal(err)
	}
	var signals *crawler.URLSignals
	if canonical != nil {
		signals = &crawler.URLSignals{Version: crawler.URLSignalsVersion, Complete: true, HTML: canonical}
	}
	if _, err := app.pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,url_signals)
 VALUES($1,$2,$3,$4,$5,200,'completed',$6)`, cfg.RunID, id, redactURL(raw), fingerprintURL(cfg.TargetFingerprintKey, raw), keyID, signals); err != nil {
		t.Fatal(err)
	}
}

func TestGEOVisibilityLookupBatchesAndCanonicalIsolation(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	reference := func(raw string) *crawler.URLReference {
		return &crawler.URLReference{Valid: true, Fingerprints: []string{hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, raw))}}
	}
	insertVisibilityLookupTarget(t, app, cfg, 3, "https://example.com/report?token=alpha", cfg.TargetFingerprintKeyID, nil)
	insertVisibilityLookupTarget(t, app, cfg, 4, "https://example.com/real", cfg.TargetFingerprintKeyID, reference("https://example.com/alias?token=unique"))
	for _, id := range []int64{5, 6} {
		insertVisibilityLookupTarget(t, app, cfg, id, fmt.Sprintf("https://example.com/many/%d", id), cfg.TargetFingerprintKeyID, reference("https://example.com/alias?token=many"))
	}
	insertVisibilityLookupTarget(t, app, cfg, 7, "https://example.com/secondary", cfg.TargetFingerprintKeyID, reference("https://example.com/report?token=beta"))
	truncated := reference("https://example.com/truncated")
	truncated.Truncated = true
	insertVisibilityLookupTarget(t, app, cfg, 8, "https://example.com/truncated-source", cfg.TargetFingerprintKeyID, truncated)
	invalid := reference("https://example.com/invalid")
	invalid.Valid = false
	insertVisibilityLookupTarget(t, app, cfg, 9, "https://example.com/invalid-source", cfg.TargetFingerprintKeyID, invalid)
	insertVisibilityLookupTarget(t, app, cfg, 10, "https://example.com/old-key", "different-key", reference("https://example.com/old-canonical"))
	other := cfg
	other.RunID = newWebRunID()
	if err := createExplicitAuditRun(t.Context(), app.pool, &other, []string{"https://example.com/other-run"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = app.pool.Exec(ctx, "DELETE FROM audit_runs WHERE id=$1", other.RunID)
	})
	insertVisibilityLookupTarget(t, app, other, 2, "https://example.com/other-run", cfg.TargetFingerprintKeyID, reference("https://example.com/other-canonical"))
	urls := []string{"https://example.com/report?token=alpha", "https://example.com/report?token=beta", "https://example.com/alias?token=unique", "https://example.com/alias?token=many", "https://example.com/truncated", "https://example.com/invalid", "https://example.com/old-key", "https://example.com/old-canonical", "https://example.com/other-run", "https://example.com/other-canonical"}
	for i := range 70 {
		raw := fmt.Sprintf("https://example.com/batch/%d", i)
		insertVisibilityLookupTarget(t, app, cfg, int64(100+i), raw, cfg.TargetFingerprintKeyID, nil)
		urls = append(urls, raw)
	}
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "gsc_ai", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", CSV: "Page,Impressions\n" + strings.Join(urls, ",1\n") + ",1\n"}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.storeVisibilityImport(t.Context(), report.ID, input, records); err != nil {
		t.Fatal(err)
	}
	want := []string{"ambiguous", "exact", "canonical_match", "ambiguous", "unmatched", "unmatched", "unmatched", "unmatched", "unmatched", "unmatched"}
	for i, record := range records {
		status := "exact"
		if i < len(want) {
			status = want[i]
		}
		if record.URLMatch != status || record.RequestURL != "" {
			t.Fatalf("row %d: %+v want=%s", i, record, status)
		}
		if status == "ambiguous" || status == "unmatched" {
			if record.TargetID != nil {
				t.Fatalf("uncertain row bound to target: %+v", record)
			}
		}
	}
	if *records[1].TargetID != 2 || *records[2].TargetID != 4 || *records[len(records)-1].TargetID != 169 {
		t.Fatalf("wrong target IDs: %+v", records)
	}
}

func TestGEOVisibilityGA4StreamsAllOriginsWithoutTruncation(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	for i := 1; i <= 130; i++ {
		path := "/other"
		if i <= 2 {
			path = "/shared"
		} else if i == 130 {
			path = "/only-last?token=omega"
		}
		raw := fmt.Sprintf("https://s%03d.example.com%s", i, path)
		insertVisibilityLookupTarget(t, app, cfg, int64(100+i), raw, cfg.TargetFingerprintKeyID, nil)
	}
	input := geo.VisibilityInput{ID: newWebRunID(), Source: "ga4", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", CSV: "Landing page,Session source,Sessions\n/only-last?token=omega,chatgpt.com,3\n/shared,chatgpt.com,4\n/not-found,example.com,1\n"}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.storeVisibilityImport(t.Context(), report.ID, input, records); err != nil {
		t.Fatal(err)
	}
	if records[0].URLMatch != "exact" || records[0].TargetID == nil || *records[0].TargetID != 230 || records[1].URLMatch != "ambiguous" || records[2].URLMatch != "unmatched" {
		t.Fatalf("origin batches lost matches: %+v", records)
	}
	if strings.Contains(records[0].URL, "omega") || records[0].RequestURL != "" {
		t.Fatalf("raw GA4 URL retained: %+v", records[0])
	}
}

func TestGEOVisibilityMigrationRecoversInterruptedIndex(t *testing.T) {
	app, report, cfg := visibilityReportFixture(t)
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, invalid := range []bool{true, false} {
		if err := goose.DownToContext(t.Context(), db, migrationDir, 46); err != nil {
			t.Fatal(err)
		}
		statement := `CREATE INDEX CONCURRENTLY audit_results_visibility_canonical_idx
 ON audit_results USING GIN ((url_signals->'html'->'fingerprints') jsonb_path_ops)
 WHERE scan_status='completed' AND status_code IS NOT NULL`
		if invalid {
			statement = `CREATE INDEX CONCURRENTLY audit_results_visibility_canonical_idx ON audit_results ((1/(target_id-target_id)))`
		}
		_, buildErr := app.pool.Exec(t.Context(), statement)
		if (buildErr != nil) != invalid {
			t.Fatalf("invalid=%t build error=%v", invalid, buildErr)
		}
		var before, after uint32
		var valid bool
		readIndex := func(oid *uint32) {
			t.Helper()
			if err := app.pool.QueryRow(t.Context(), `SELECT indexrelid,indisvalid FROM pg_index
 WHERE indexrelid=to_regclass('audit_results_visibility_canonical_idx')`).Scan(oid, &valid); err != nil {
				t.Fatal(err)
			}
		}
		readIndex(&before)
		if valid == invalid {
			t.Fatalf("unexpected index validity before recovery: %t", valid)
		}
		if err := applySchemaMigrations(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
		readIndex(&after)
		if !valid || (!invalid && before != after) || (invalid && before == after) {
			t.Fatalf("wrong recovery: invalid=%t valid=%t old=%d new=%d", invalid, valid, before, after)
		}
		var results, reports int
		if err := app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&results); err != nil {
			t.Fatal(err)
		}
		if err := app.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM geo_reports WHERE id=$1", report.ID).Scan(&reports); err != nil || results != 2 || reports != 1 {
			t.Fatalf("recovery changed reports: results=%d reports=%d err=%v", results, reports, err)
		}
	}
}
