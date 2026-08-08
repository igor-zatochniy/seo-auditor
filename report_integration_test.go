//go:build integration

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExportAuditReportFromPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	applyIntegrationMigrations(t, ctx, databaseURL)

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	const runID = "a94e81a5-97ce-47cb-811f-d582580a19df"
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	startedAt := time.Date(2026, time.August, 5, 11, 30, 0, 0, time.UTC)
	finishedAt := startedAt.Add(1500 * time.Millisecond)
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_runs (
			id, started_at, finished_at, status, total_urls, successful_urls, failed_urls,
			heartbeat_at, worker_instance_id, targets_captured_at
		 ) VALUES ($1, $2, $3, $4, 1, 1, 0, $3, 'report-integration-test', $2)`,
		runID,
		startedAt,
		finishedAt,
		auditRunStatusCompleted,
	); err != nil {
		t.Fatalf("insert audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (
			run_id, target_id, request_url, request_url_cleared_at, status, attempts, finished_at
		 ) VALUES ($1, 17, '', $2, 'completed', 1, $2)`,
		runID,
		finishedAt,
	); err != nil {
		t.Fatalf("insert audit target: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_results (
			run_id, target_id, safe_url, target_fingerprint, fingerprint_key_id,
			status_code, scan_status, title, description, h1,
			internal_links_count, external_links_count, links_count, images_missing_alt,
			meta_robots, x_robots_tag, robots_allowed, robots_outcome,
			word_count, duration_ms, error_code, error_message
		 ) VALUES (
			$1, 17, $2, $3, 'integration',
			200, 'completed', $4, 'Integration description', 'Integration H1',
			4, 2, 6, 1,
			'index,follow', '', TRUE, 'allowed',
			321, 1450, '', ''
		 )`,
		runID,
		"https://example.com/page?token=[REDACTED]",
		[]byte("report-integration-fingerprint"),
		`Integration <script>alert("title")</script>`,
	); err != nil {
		t.Fatalf("insert audit result: %v", err)
	}

	generatedAt := finishedAt.Add(time.Second)
	paths, err := exportAuditReport(ctx, pool, runID, t.TempDir(), generatedAt)
	if err != nil {
		t.Fatalf("export audit report: %v", err)
	}
	latest, err := os.ReadFile(paths.Latest)
	if err != nil {
		t.Fatalf("read latest audit report: %v", err)
	}
	if _, err := os.Stat(paths.Archive); err != nil {
		t.Fatalf("stat archived audit report: %v", err)
	}

	report := string(latest)
	for _, expected := range []string{
		runID,
		"https://example.com/page?token=[REDACTED]",
		"Integration description",
		"Integration H1",
		"Внутрішні: 4",
		"Результат: дозволено",
		"321",
		"1.45 s",
		`Integration &lt;script&gt;alert(&#34;title&#34;)&lt;/script&gt;`,
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("generated report is missing %q", expected)
		}
	}
	if strings.Contains(report, `<script>alert("title")</script>`) {
		t.Fatal("generated report contains unescaped HTML from PostgreSQL")
	}
}
