//go:build integration

package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func TestAuditPipelinePersistsResult(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head><title>Integration audit result page title</title></head><body><h1>Verified pipeline</h1></body></html>")
	}))
	defer server.Close()

	const firstRunID = "d2bc9bae-6bcd-4e85-9b56-fb0707488cc7"
	const secondRunID = "517637b3-b45f-4b52-a982-78fdca30a4e4"
	const collisionRunID = "305beb1e-38d0-421d-8b85-6fefc2debbf5"
	targetURL := server.URL + "/page"
	deleteTestRuns := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(
			cleanupCtx,
			"DELETE FROM audit_runs WHERE id IN ($1::UUID, $2::UUID, $3::UUID)",
			firstRunID,
			secondRunID,
			collisionRunID,
		)
	}
	deleteTestRuns(ctx)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		deleteTestRuns(cleanupCtx)
	})

	newConfig := func(runID string) Config {
		return Config{
			RunID:                runID,
			TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
			HTTPAttemptTimeout:   2 * time.Second,
			HTTPTotalTimeout:     5 * time.Second,
			RobotsAttemptTimeout: time.Second,
			RobotsTotalTimeout:   5 * time.Second,
			DBWriteTimeout:       3 * time.Second,
			MaxHTMLBodyBytes:     DefaultMaxHTMLBodyBytes,
			DBMaxRetries:         2,
			RetryBaseDelay:       10 * time.Millisecond,
			RetryMaxDelay:        50 * time.Millisecond,
		}
	}
	createRunTarget := func(cfg Config, targetID int64, requestURL string) AuditTarget {
		t.Helper()
		target := newAuditTarget(targetURLRecord{ID: targetID, URL: requestURL}, requestURL, cfg.TargetFingerprintKey)
		if _, err := pool.Exec(
			ctx,
			`INSERT INTO audit_run_targets (run_id, target_id, request_url)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (run_id, target_id) DO UPDATE
			 SET request_url = EXCLUDED.request_url`,
			cfg.RunID,
			target.TargetID,
			target.RequestURL,
		); err != nil {
			t.Fatalf("create audit run target: %v", err)
		}
		return target
	}
	persistPipelineResult := func(cfg Config, target AuditTarget) ResultSummary {
		t.Helper()
		jobs := make(chan AuditTarget, 1)
		jobs <- target
		close(jobs)
		results := make(chan Result, 1)

		var workers sync.WaitGroup
		workers.Add(1)
		go worker(
			ctx,
			ctx,
			1,
			jobs,
			results,
			server.Client(),
			server.Client(),
			newRobotsPolicyCache(time.Minute, 16),
			pool,
			cfg,
			&workers,
		)
		go func() {
			workers.Wait()
			close(results)
		}()

		return saveResults(ctx, pool, results, cfg)
	}

	firstConfig := newConfig(firstRunID)
	if err := createAuditRun(ctx, pool, firstConfig); err != nil {
		t.Fatalf("create first audit run: %v", err)
	}
	firstTarget := createRunTarget(firstConfig, 1, targetURL)
	firstSummary := persistPipelineResult(firstConfig, firstTarget)
	if firstSummary.Saved != 1 || firstSummary.Successful != 1 || firstSummary.Failed != 0 {
		t.Fatalf("unexpected first save summary: %+v", firstSummary)
	}
	var firstTargetStatus string
	var firstTargetAttempts int
	var firstTargetClaimedBy string
	if err := pool.QueryRow(
		ctx,
		`SELECT status, attempts, COALESCE(claimed_by, '')
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = $2`,
		firstRunID,
		firstTarget.TargetID,
	).Scan(&firstTargetStatus, &firstTargetAttempts, &firstTargetClaimedBy); err != nil {
		t.Fatalf("read first target progress: %v", err)
	}
	if firstTargetStatus != auditTargetStatusCompleted || firstTargetAttempts != 1 || firstTargetClaimedBy == "" {
		t.Fatalf("unexpected first target progress: status=%q attempts=%d claimed_by=%q", firstTargetStatus, firstTargetAttempts, firstTargetClaimedBy)
	}

	updatedResults := make(chan Result, 1)
	updatedResults <- Result{Target: firstTarget, Data: SEOData{
		URL:           targetURL,
		StatusCode:    httpStatus(http.StatusOK),
		ScanStatus:    scanStatusCompleted,
		Title:         "Updated result from the same run",
		RobotsAllowed: true,
		RobotsOutcome: robotsOutcomeAllowed,
	}}
	close(updatedResults)
	updatedSummary := saveResults(ctx, pool, updatedResults, firstConfig)
	if updatedSummary.Saved != 1 || updatedSummary.Successful != 1 || updatedSummary.Failed != 0 {
		t.Fatalf("unexpected same-run update summary: %+v", updatedSummary)
	}
	if err := completeAuditRun(ctx, pool, firstRunID, auditRunCompletion{
		Status:         auditRunStatusCompleted,
		TotalURLs:      1,
		SuccessfulURLs: firstSummary.Successful,
		FailedURLs:     firstSummary.Failed,
	}, firstConfig); err != nil {
		t.Fatalf("complete first audit run: %v", err)
	}

	secondConfig := newConfig(secondRunID)
	if err := createAuditRun(ctx, pool, secondConfig); err != nil {
		t.Fatalf("create second audit run: %v", err)
	}
	secondTarget := createRunTarget(secondConfig, 1, targetURL)
	secondSummary := persistPipelineResult(secondConfig, secondTarget)
	if secondSummary.Saved != 1 || secondSummary.Successful != 1 || secondSummary.Failed != 0 {
		t.Fatalf("unexpected second save summary: %+v", secondSummary)
	}
	if err := completeAuditRun(ctx, pool, secondRunID, auditRunCompletion{
		Status:         auditRunStatusCompleted,
		TotalURLs:      1,
		SuccessfulURLs: secondSummary.Successful,
		FailedURLs:     secondSummary.Failed,
	}, secondConfig); err != nil {
		t.Fatalf("complete second audit run: %v", err)
	}

	var resultCount int
	err = pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		 FROM audit_results
		 WHERE safe_url = $1 AND run_id IN ($2::UUID, $3::UUID)`,
		redactURL(targetURL),
		firstRunID,
		secondRunID,
	).Scan(&resultCount)
	if err != nil {
		t.Fatalf("count persisted audit history: %v", err)
	}
	if resultCount != 2 {
		t.Fatalf("expected two run-specific results, got %d", resultCount)
	}

	var firstTitle, secondTitle string
	if err := pool.QueryRow(
		ctx,
		"SELECT title FROM audit_results WHERE run_id = $1 AND safe_url = $2",
		firstRunID,
		redactURL(targetURL),
	).Scan(&firstTitle); err != nil {
		t.Fatalf("read first audit result: %v", err)
	}
	if err := pool.QueryRow(
		ctx,
		"SELECT title FROM audit_results WHERE run_id = $1 AND safe_url = $2",
		secondRunID,
		redactURL(targetURL),
	).Scan(&secondTitle); err != nil {
		t.Fatalf("read second audit result: %v", err)
	}
	if firstTitle != "Updated result from the same run" {
		t.Fatalf("same-run upsert did not update the first result: %q", firstTitle)
	}
	if secondTitle != "Integration audit result page title" {
		t.Fatalf("second run did not preserve an independent result: %q", secondTitle)
	}

	collisionConfig := newConfig(collisionRunID)
	if err := createAuditRun(ctx, pool, collisionConfig); err != nil {
		t.Fatalf("create collision audit run: %v", err)
	}
	signedTargetA := createRunTarget(collisionConfig, 1, targetURL+"?token=AAAA")
	signedTargetB := createRunTarget(collisionConfig, 2, targetURL+"?token=BBBB")
	signedResults := make(chan Result, 2)
	signedResults <- Result{Target: signedTargetA, Data: SEOData{
		URL:           targetURL + "?token=AAAA",
		StatusCode:    httpStatus(http.StatusOK),
		ScanStatus:    scanStatusCompleted,
		Title:         "Signed URL A",
		RobotsAllowed: true,
		RobotsOutcome: robotsOutcomeAllowed,
	}}
	signedResults <- Result{Target: signedTargetB, Data: SEOData{
		URL:           targetURL + "?token=BBBB",
		StatusCode:    httpStatus(http.StatusOK),
		ScanStatus:    scanStatusCompleted,
		Title:         "Signed URL B",
		RobotsAllowed: true,
		RobotsOutcome: robotsOutcomeAllowed,
	}}
	close(signedResults)
	collisionSummary := saveResults(ctx, pool, signedResults, collisionConfig)
	if collisionSummary.Saved != 2 || collisionSummary.Successful != 2 || collisionSummary.Failed != 0 {
		t.Fatalf("unexpected collision save summary: %+v", collisionSummary)
	}
	if err := completeAuditRun(ctx, pool, collisionRunID, auditRunCompletion{
		Status:         auditRunStatusCompleted,
		TotalURLs:      2,
		SuccessfulURLs: collisionSummary.Successful,
		FailedURLs:     collisionSummary.Failed,
	}, collisionConfig); err != nil {
		t.Fatalf("complete collision audit run: %v", err)
	}

	var collisionRows, collisionFingerprints int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*), COUNT(DISTINCT target_fingerprint)
		 FROM audit_results
		 WHERE run_id = $1 AND safe_url = $2`,
		collisionRunID,
		redactURL(targetURL+"?token=AAAA"),
	).Scan(&collisionRows, &collisionFingerprints); err != nil {
		t.Fatalf("read signed URL collision results: %v", err)
	}
	if collisionRows != 2 || collisionFingerprints != 2 {
		t.Fatalf("signed URL results collided: rows=%d fingerprints=%d", collisionRows, collisionFingerprints)
	}

	var collisionTargets int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(DISTINCT target_id)
		 FROM audit_results
		 WHERE run_id = $1 AND safe_url = $2`,
		collisionRunID,
		redactURL(targetURL+"?token=AAAA"),
	).Scan(&collisionTargets); err != nil {
		t.Fatalf("read signed URL target links: %v", err)
	}
	if collisionTargets != 2 {
		t.Fatalf("signed URL results lost target links: target_ids=%d", collisionTargets)
	}

	var retainedRequestURLs int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		 FROM audit_run_targets
		 WHERE run_id = $1::UUID
		   AND request_url <> ''`,
		collisionRunID,
	).Scan(&retainedRequestURLs); err != nil {
		t.Fatalf("count retained snapshot request URLs: %v", err)
	}
	if retainedRequestURLs != 0 {
		t.Fatalf("completed audit run retained raw request URLs: %d", retainedRequestURLs)
	}

	var fingerprintKeyIDs int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		 FROM audit_results
		 WHERE run_id = $1::UUID
		   AND fingerprint_key_id <> ''`,
		collisionRunID,
	).Scan(&fingerprintKeyIDs); err != nil {
		t.Fatalf("count fingerprint key IDs: %v", err)
	}
	if fingerprintKeyIDs != 2 {
		t.Fatalf("expected fingerprint key IDs for signed URL results, got %d", fingerprintKeyIDs)
	}

	var runStatus string
	var totalURLs, successfulURLs, failedURLs int
	if err := pool.QueryRow(
		ctx,
		`SELECT status, total_urls, successful_urls, failed_urls
		 FROM audit_runs
		 WHERE id = $1`,
		secondRunID,
	).Scan(&runStatus, &totalURLs, &successfulURLs, &failedURLs); err != nil {
		t.Fatalf("read completed audit run: %v", err)
	}
	if runStatus != auditRunStatusCompleted || totalURLs != 1 || successfulURLs != 1 || failedURLs != 0 {
		t.Fatalf(
			"unexpected audit run summary: status=%q total=%d successful=%d failed=%d",
			runStatus,
			totalURLs,
			successfulURLs,
			failedURLs,
		)
	}
}

func TestAuditRunTargetSnapshotIsStable(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	const runID = "9f5f1a07-0ed1-4d3f-9b1f-3f5b08bc7f10"
	const sourceA = "https://stability-check.example/a"
	const sourceB = "https://stability-check.example/b"
	const sourceC = "https://stability-check.example/c"

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_run_targets WHERE run_id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM pages_to_scan WHERE url IN ($1, $2, $3)", sourceA, sourceB, sourceC)
	}
	cleanup(ctx)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	})

	cfg := Config{
		RunID:                runID,
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		DBFetchTimeout:       3 * time.Second,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}

	if _, err := pool.Exec(
		ctx,
		`INSERT INTO pages_to_scan (url, is_active)
		 VALUES ($1, TRUE), ($2, TRUE)
		 ON CONFLICT (url) DO UPDATE
		 SET is_active = EXCLUDED.is_active`,
		sourceA,
		sourceB,
	); err != nil {
		t.Fatalf("seed source URLs: %v", err)
	}

	if err := createAuditRun(ctx, pool, cfg); err != nil {
		t.Fatalf("create audit run: %v", err)
	}
	snapshot, err := captureAuditRunTargets(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("capture audit run targets: %v", err)
	}
	if snapshot.Total != 2 {
		t.Fatalf("unexpected target snapshot total: %d", snapshot.Total)
	}

	if _, err := pool.Exec(ctx, `UPDATE pages_to_scan SET is_active = FALSE WHERE url = $1`, sourceB); err != nil {
		t.Fatalf("deactivate source URL: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO pages_to_scan (url, is_active)
		 VALUES ($1, TRUE)
		 ON CONFLICT (url) DO UPDATE
		 SET is_active = EXCLUDED.is_active`,
		sourceC,
	); err != nil {
		t.Fatalf("add late source URL: %v", err)
	}

	jobs := make(chan AuditTarget, 4)
	invalidResults := make(chan Result, 1)
	streamSummary := streamTargetURLs(
		ctx,
		snapshot.HighWatermark,
		2,
		cfg,
		jobs,
		invalidResults,
		func(batchCtx context.Context, afterID, highWatermark int64, limit int) ([]targetURLRecord, error) {
			return fetchTargetURLBatch(batchCtx, pool, cfg, afterID, highWatermark, limit)
		},
	)
	close(jobs)
	close(invalidResults)

	if streamSummary.Error != nil {
		t.Fatalf("stream stable targets: %v", streamSummary.Error)
	}
	if streamSummary.Queued != 2 || streamSummary.Skipped != 0 {
		t.Fatalf("unexpected stream summary: %+v", streamSummary)
	}

	got := make(map[string]struct{})
	for target := range jobs {
		got[target.RequestURL] = struct{}{}
	}
	if len(got) != 2 {
		t.Fatalf("unexpected stable target count: %d", len(got))
	}
	if _, ok := got[sourceA]; !ok {
		t.Fatalf("stable snapshot lost active source URL %q", sourceA)
	}
	if _, ok := got[sourceB]; !ok {
		t.Fatalf("stable snapshot lost deactivated source URL %q", sourceB)
	}
	if _, ok := got[sourceC]; ok {
		t.Fatalf("late source URL %q leaked into stable snapshot", sourceC)
	}

	var stableCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_run_targets WHERE run_id = $1`, runID).Scan(&stableCount); err != nil {
		t.Fatalf("count stable targets: %v", err)
	}
	if stableCount != 2 {
		t.Fatalf("unexpected persisted stable target count: %d", stableCount)
	}
}

func TestAbandonStaleAuditRunsMarksRunAndTargets(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	const runID = "45e3b1af-b883-4214-a2d6-789d42c16fa8"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "integration-worker",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		StaleRunThreshold:    time.Minute,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	})

	if err := createAuditRun(ctx, pool, cfg); err != nil {
		t.Fatalf("create stale audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url, status, attempts, claimed_by, claimed_at)
		 VALUES ($1, 1, 'https://example.com/stale', $2, 1, $3, CURRENT_TIMESTAMP)`,
		runID,
		auditTargetStatusRunning,
		cfg.WorkerInstanceID,
	); err != nil {
		t.Fatalf("insert stale audit target: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET heartbeat_at = CURRENT_TIMESTAMP - INTERVAL '10 minutes'
		 WHERE id = $1`,
		runID,
	); err != nil {
		t.Fatalf("age audit run heartbeat: %v", err)
	}

	abandoned, err := abandonStaleAuditRuns(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("abandon stale audit runs: %v", err)
	}
	if abandoned != 1 {
		t.Fatalf("unexpected abandoned run count: %d", abandoned)
	}

	var runStatus string
	var finished bool
	if err := pool.QueryRow(
		ctx,
		`SELECT status, finished_at IS NOT NULL
		 FROM audit_runs
		 WHERE id = $1`,
		runID,
	).Scan(&runStatus, &finished); err != nil {
		t.Fatalf("read abandoned audit run: %v", err)
	}
	if runStatus != auditRunStatusAbandoned || !finished {
		t.Fatalf("unexpected abandoned run state: status=%q finished=%t", runStatus, finished)
	}

	var targetStatus string
	var lastError string
	if err := pool.QueryRow(
		ctx,
		`SELECT status, last_error
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = 1`,
		runID,
	).Scan(&targetStatus, &lastError); err != nil {
		t.Fatalf("read abandoned audit target: %v", err)
	}
	if targetStatus != auditTargetStatusAbandoned || lastError == "" {
		t.Fatalf("unexpected abandoned target state: status=%q last_error=%q", targetStatus, lastError)
	}
}

func TestMigrationsUpgradeLegacyAuditResults(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminDB, err := sql.Open(postgresMigrationDriver, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL admin connection: %v", err)
	}
	defer adminDB.Close()
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL admin connection: %v", err)
	}

	schemaName := fmt.Sprintf("migration_upgrade_%d", time.Now().UnixNano())
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+quoteSQLIdentifier(schemaName)); err != nil {
		t.Fatalf("create temporary schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = adminDB.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+quoteSQLIdentifier(schemaName)+` CASCADE`)
	})

	migrationDB, err := sql.Open(postgresMigrationDriver, withSearchPath(databaseURL, schemaName))
	if err != nil {
		t.Fatalf("open migration connection: %v", err)
	}
	defer migrationDB.Close()
	migrationDB.SetMaxOpenConns(1)
	migrationDB.SetMaxIdleConns(1)

	var currentSchema string
	if err := migrationDB.QueryRowContext(ctx, "SELECT current_schema()").Scan(&currentSchema); err != nil {
		t.Fatalf("read current schema: %v", err)
	}
	if currentSchema != schemaName {
		t.Fatalf("migration connection uses schema %q, want %q", currentSchema, schemaName)
	}

	goose.SetBaseFS(migrationFiles)
	goose.SetTableName(migrationVersionTable)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(postgresMigrationDriver); err != nil {
		t.Fatalf("configure goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, migrationDir, 2); err != nil {
		t.Fatalf("apply baseline migrations: %v", err)
	}

	const runID = "48025f74-f8d1-4055-b548-b7d19d92965c"
	const legacyURL = "https://example.com/report?token=real-secret&view=full#secret-fragment"
	const sanitizedLegacyURL = "https://example.com/report?[QUERY_REDACTED]"
	if _, err := migrationDB.ExecContext(
		ctx,
		`INSERT INTO audit_runs (id, started_at, finished_at, status, total_urls, successful_urls, failed_urls)
		 VALUES ($1::UUID, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'completed', 1, 1, 0)`,
		runID,
	); err != nil {
		t.Fatalf("insert legacy audit run: %v", err)
	}
	if _, err := migrationDB.ExecContext(
		ctx,
		`INSERT INTO audit_results (run_id, url, status_code, scan_status, title)
		 VALUES ($1::UUID, $2, 200, 'completed', 'Legacy result')`,
		runID,
		legacyURL,
	); err != nil {
		t.Fatalf("insert legacy audit result: %v", err)
	}

	if err := applySchemaMigrationsDB(ctx, migrationDB); err != nil {
		t.Fatalf("apply target-linked migration: %v", err)
	}

	var targetID int64
	var title string
	var fingerprintKeyID string
	if err := migrationDB.QueryRowContext(
		ctx,
		`SELECT target_id, title, fingerprint_key_id
		 FROM audit_results
		 WHERE run_id = $1::UUID AND safe_url = $2`,
		runID,
		sanitizedLegacyURL,
	).Scan(&targetID, &title, &fingerprintKeyID); err != nil {
		t.Fatalf("read upgraded audit result: %v", err)
	}
	if targetID >= 0 {
		t.Fatalf("legacy result target_id should be synthetic and negative, got %d", targetID)
	}
	if title != "Legacy result" {
		t.Fatalf("legacy result was not preserved: %q", title)
	}
	if fingerprintKeyID != "legacy" {
		t.Fatalf("unexpected legacy fingerprint key ID: %q", fingerprintKeyID)
	}

	var requestURL string
	var requestURLCleared bool
	if err := migrationDB.QueryRowContext(
		ctx,
		`SELECT request_url, request_url_cleared_at IS NOT NULL
		 FROM audit_run_targets
		 WHERE run_id = $1::UUID AND target_id = $2`,
		runID,
		targetID,
	).Scan(&requestURL, &requestURLCleared); err != nil {
		t.Fatalf("read upgraded audit target: %v", err)
	}
	if requestURL != "" || !requestURLCleared {
		t.Fatalf("legacy request URL was not cleared: request_url=%q cleared=%t", requestURL, requestURLCleared)
	}
}

func withSearchPath(databaseURL, schemaName string) string {
	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	return databaseURL + separator + "search_path=" + url.QueryEscape(schemaName)
}

func quoteSQLIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func applyIntegrationMigrations(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	if err := applySchemaMigrations(ctx, Config{DatabaseURL: databaseURL}); err != nil {
		t.Fatalf("apply PostgreSQL migrations: %v", err)
	}
}
