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

	"github.com/jackc/pgx/v5"
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
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		deleteTestRuns(cleanupCtx)
	}()

	newConfig := func(runID string) Config {
		return Config{
			RunID:                runID,
			TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
			HTTPAttemptTimeout:   2 * time.Second,
			HTTPTotalTimeout:     5 * time.Second,
			RobotsAttemptTimeout: time.Second,
			RobotsTotalTimeout:   5 * time.Second,
			DBFetchTimeout:       3 * time.Second,
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
		claimed, err := claimTargetURLBatch(ctx, pool, cfg, 1)
		if err != nil {
			t.Fatalf("claim audit run target: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != targetID {
			t.Fatalf("unexpected claimed audit target: %#v", claimed)
		}
		var attempts int
		var started bool
		if err := pool.QueryRow(
			ctx,
			`SELECT attempts, started_at IS NOT NULL
			 FROM audit_run_targets
			 WHERE run_id = $1 AND target_id = $2`,
			cfg.RunID,
			targetID,
		).Scan(&attempts, &started); err != nil {
			t.Fatalf("read claimed target progress: %v", err)
		}
		if attempts != 0 || started {
			t.Fatalf("claim counted an attempt before worker start: attempts=%d started=%t", attempts, started)
		}
		return newAuditTarget(claimed[0], claimed[0].URL, cfg.TargetFingerprintKey)
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
	if err := createAuditRun(ctx, pool, &firstConfig); err != nil {
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
	var firstTargetStarted bool
	if err := pool.QueryRow(
		ctx,
		`SELECT status, attempts, COALESCE(claimed_by, ''), started_at IS NOT NULL
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = $2`,
		firstRunID,
		firstTarget.TargetID,
	).Scan(&firstTargetStatus, &firstTargetAttempts, &firstTargetClaimedBy, &firstTargetStarted); err != nil {
		t.Fatalf("read first target progress: %v", err)
	}
	if firstTargetStatus != auditTargetStatusCompleted || firstTargetAttempts != 1 || firstTargetClaimedBy == "" || !firstTargetStarted {
		t.Fatalf(
			"unexpected first target progress: status=%q attempts=%d claimed_by=%q started=%t",
			firstTargetStatus,
			firstTargetAttempts,
			firstTargetClaimedBy,
			firstTargetStarted,
		)
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
	if updatedSummary.Saved != 0 ||
		updatedSummary.Successful != 0 ||
		updatedSummary.Failed != 1 ||
		updatedSummary.PersistenceFailures != 1 {
		t.Fatalf("unclaimed same-run update was not rejected: %+v", updatedSummary)
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
	if err := createAuditRun(ctx, pool, &secondConfig); err != nil {
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
	if firstTitle != "Integration audit result page title" {
		t.Fatalf("unclaimed same-run update changed the first result: %q", firstTitle)
	}
	if secondTitle != "Integration audit result page title" {
		t.Fatalf("second run did not preserve an independent result: %q", secondTitle)
	}

	collisionConfig := newConfig(collisionRunID)
	if err := createAuditRun(ctx, pool, &collisionConfig); err != nil {
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
	restoreActivePages := suspendActivePages(t, ctx, pool)
	defer restoreActivePages()

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_run_targets WHERE run_id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM pages_to_scan WHERE url IN ($1, $2, $3)", sourceA, sourceB, sourceC)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

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

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
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
		2,
		cfg,
		jobs,
		invalidResults,
		func(batchCtx context.Context, limit int) ([]targetURLRecord, error) {
			return claimTargetURLBatch(batchCtx, pool, cfg, limit)
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

func TestTargetSnapshotUsesPerBatchWriteTimeout(t *testing.T) {
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

	const runID = "006d8998-a43f-452c-ac7f-91919259db23"
	const triggerName = "integration_delay_snapshot_target"
	restoreActivePages := suspendActivePages(t, ctx, pool)
	defer restoreActivePages()

	urls := make([]string, 8)
	for index := range urls {
		urls[index] = fmt.Sprintf("https://bounded-snapshot.example/page-%d", index)
	}
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS integration_delay_snapshot_target ON audit_run_targets`)
		_, _ = pool.Exec(cleanupCtx, `DROP FUNCTION IF EXISTS integration_delay_snapshot_target()`)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_runs WHERE id = $1`, runID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pages_to_scan WHERE url = ANY($1::TEXT[])`, urls)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	for _, targetURL := range urls {
		if _, err := pool.Exec(
			ctx,
			`INSERT INTO pages_to_scan (url, is_active)
			 VALUES ($1, TRUE)
			 ON CONFLICT (url) DO UPDATE SET is_active = TRUE`,
			targetURL,
		); err != nil {
			t.Fatalf("seed bounded snapshot URL: %v", err)
		}
	}
	if _, err := pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE FUNCTION %s()
			 RETURNS TRIGGER
			 LANGUAGE plpgsql
			 AS $$
			 BEGIN
			     IF NEW.run_id = '%s'::UUID THEN
			         PERFORM pg_sleep(0.15);
			     END IF;
			     RETURN NEW;
			 END
			 $$`,
			triggerName,
			runID,
		),
	); err != nil {
		t.Fatalf("create snapshot delay function: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`CREATE TRIGGER integration_delay_snapshot_target
		 BEFORE INSERT ON audit_run_targets
		 FOR EACH ROW
		 EXECUTE FUNCTION integration_delay_snapshot_target()`,
	); err != nil {
		t.Fatalf("create snapshot delay trigger: %v", err)
	}

	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "bounded-snapshot-worker",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		URLBatchSize:         2,
		DBWriteTimeout:       800 * time.Millisecond,
		DBFetchTimeout:       time.Second,
	}
	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create bounded snapshot audit run: %v", err)
	}

	snapshot, err := captureAuditRunTargets(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("capture bounded audit target snapshot: %v", err)
	}
	if snapshot.Total != int64(len(urls)) {
		t.Fatalf("unexpected bounded snapshot total: got %d want %d", snapshot.Total, len(urls))
	}
}

func TestAuditRunCompletionUsesPerBatchWriteTimeout(t *testing.T) {
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

	const runID = "62bcc470-d79d-4939-916f-822384b24b36"
	const triggerName = "integration_delay_target_cleanup"
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS integration_delay_target_cleanup ON audit_run_targets`)
		_, _ = pool.Exec(cleanupCtx, `DROP FUNCTION IF EXISTS integration_delay_target_cleanup()`)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_runs WHERE id = $1`, runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "bounded-completion-worker",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		URLBatchSize:         2,
		DBWriteTimeout:       800 * time.Millisecond,
		DBFetchTimeout:       time.Second,
	}
	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create bounded completion audit run: %v", err)
	}
	for targetID := int64(1); targetID <= 8; targetID++ {
		if _, err := pool.Exec(
			ctx,
			`INSERT INTO audit_run_targets (
			     run_id, target_id, request_url, status, finished_at
			 ) VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)`,
			runID,
			targetID,
			fmt.Sprintf("https://bounded-completion.example/page-%d", targetID),
			auditTargetStatusCompleted,
		); err != nil {
			t.Fatalf("insert completed audit target: %v", err)
		}
	}
	if _, err := pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE FUNCTION %s()
			 RETURNS TRIGGER
			 LANGUAGE plpgsql
			 AS $$
			 BEGIN
			     IF OLD.run_id = '%s'::UUID
			        AND OLD.request_url <> ''
			        AND NEW.request_url = '' THEN
			         PERFORM pg_sleep(0.15);
			     END IF;
			     RETURN NEW;
			 END
			 $$`,
			triggerName,
			runID,
		),
	); err != nil {
		t.Fatalf("create completion delay function: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`CREATE TRIGGER integration_delay_target_cleanup
		 BEFORE UPDATE ON audit_run_targets
		 FOR EACH ROW
		 EXECUTE FUNCTION integration_delay_target_cleanup()`,
	); err != nil {
		t.Fatalf("create completion delay trigger: %v", err)
	}

	if err := completeAuditRun(ctx, pool, runID, auditRunCompletion{
		Status:         auditRunStatusCompleted,
		TotalURLs:      8,
		SuccessfulURLs: 8,
	}, cfg); err != nil {
		t.Fatalf("complete audit run in bounded batches: %v", err)
	}
	if err := completeAuditRun(ctx, pool, runID, auditRunCompletion{
		Status:         auditRunStatusCompleted,
		TotalURLs:      8,
		SuccessfulURLs: 8,
	}, cfg); err != nil {
		t.Fatalf("repeat idempotent audit run completion: %v", err)
	}

	var status string
	var retainedURLs int
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status, COUNT(*) FILTER (WHERE target.request_url <> '')
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1
		 GROUP BY run.status`,
		runID,
	).Scan(&status, &retainedURLs); err != nil {
		t.Fatalf("read bounded completion state: %v", err)
	}
	if status != auditRunStatusCompleted || retainedURLs != 0 {
		t.Fatalf("unexpected bounded completion state: status=%q retained_urls=%d", status, retainedURLs)
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
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
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
	if abandoned < 1 {
		t.Fatalf("expected at least one abandoned run, got %d", abandoned)
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

func TestAbandonLargeStaleAuditRunUsesBoundedBatches(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	applyIntegrationMigrations(t, ctx, databaseURL)

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL pool: %v", err)
	}
	defer pool.Close()

	const runID = "4d271a71-45f4-4c93-b56c-e9a67f17f001"
	const targetCount = int64(250_000)
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	cfg := Config{
		RunID:                     runID,
		WorkerInstanceID:          "large-stale-recovery-worker",
		TargetFingerprintKey:      []byte("local-development-only-fingerprint-key"),
		URLBatchSize:              MaxURLBatchSize,
		DBFetchTimeout:            3 * time.Second,
		DBWriteTimeout:            3 * time.Second,
		StaleRecoveryBatchTimeout: 15 * time.Second,
		StaleRunThreshold:         time.Minute,
		DBMaxRetries:              0,
		RetryBaseDelay:            10 * time.Millisecond,
		RetryMaxDelay:             50 * time.Millisecond,
	}
	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create large stale audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url, status)
		 SELECT $1, target_id, 'https://large-stale.example/page/' || target_id, $2
		 FROM generate_series(1, $3) AS target_id`,
		runID,
		auditTargetStatusPending,
		targetCount,
	); err != nil {
		t.Fatalf("insert large stale target set: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET heartbeat_at = CURRENT_TIMESTAMP - INTERVAL '10 minutes'
		 WHERE id = $1`,
		runID,
	); err != nil {
		t.Fatalf("age large audit run heartbeat: %v", err)
	}

	abandoned, err := abandonStaleAuditRuns(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("abandon large stale audit run in bounded batches: %v", err)
	}
	if abandoned < 1 {
		t.Fatalf("expected the large stale run to be abandoned, got %d transitioned runs", abandoned)
	}

	var runStatus string
	var abandonedTargets int64
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status,
		        COUNT(*) FILTER (WHERE target.status = $2)
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1
		 GROUP BY run.status`,
		runID,
		auditTargetStatusAbandoned,
	).Scan(&runStatus, &abandonedTargets); err != nil {
		t.Fatalf("read large stale recovery state: %v", err)
	}
	if runStatus != auditRunStatusAbandoned || abandonedTargets != targetCount {
		t.Fatalf(
			"unexpected large stale recovery state: status=%q abandoned_targets=%d want=%d",
			runStatus,
			abandonedTargets,
			targetCount,
		)
	}
}

func TestAbandonStaleAuditRunsContinuesInterruptedTargetRecovery(t *testing.T) {
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

	const runID = "dc588a56-c86d-497c-bcab-1bb74a179c18"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "interrupted-stale-recovery-worker",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		URLBatchSize:         2,
		DBFetchTimeout:       time.Second,
		DBWriteTimeout:       time.Second,
		StaleRunThreshold:    time.Minute,
		RetryBaseDelay:       time.Millisecond,
		RetryMaxDelay:        5 * time.Millisecond,
	}
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create interrupted stale audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (
		     run_id, target_id, request_url, status, claimed_by, claimed_at, lease_until, finished_at
		 ) VALUES
		     ($1, 1, 'https://interrupted.example/1', $2, NULL, NULL, NULL, CURRENT_TIMESTAMP),
		     ($1, 2, 'https://interrupted.example/2', $3, NULL, NULL, NULL, NULL),
		     ($1, 3, 'https://interrupted.example/3', $4, $8, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + INTERVAL '1 minute', NULL),
		     ($1, 4, 'https://interrupted.example/4', $5, NULL, NULL, NULL, CURRENT_TIMESTAMP),
		     ($1, 5, 'https://interrupted.example/5', $6, NULL, NULL, NULL, CURRENT_TIMESTAMP),
		     ($1, 6, 'https://interrupted.example/6', $7, NULL, NULL, NULL, CURRENT_TIMESTAMP)`,
		runID,
		auditTargetStatusAbandoned,
		auditTargetStatusPending,
		auditTargetStatusRunning,
		auditTargetStatusCompleted,
		auditTargetStatusFailed,
		auditTargetStatusCanceled,
		cfg.WorkerInstanceID,
	); err != nil {
		t.Fatalf("insert interrupted stale targets: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET status = $2,
		     finished_at = CURRENT_TIMESTAMP
		 WHERE id = $1`,
		runID,
		auditRunStatusAbandoned,
	); err != nil {
		t.Fatalf("simulate interruption after stale run transition: %v", err)
	}

	if _, err := abandonStaleAuditRuns(ctx, pool, cfg); err != nil {
		t.Fatalf("continue interrupted stale target recovery: %v", err)
	}
	if _, err := abandonStaleAuditRuns(ctx, pool, cfg); err != nil {
		t.Fatalf("repeat interrupted stale target recovery idempotently: %v", err)
	}

	var abandonedTargets int
	var completedTargets int
	var failedTargets int
	var canceledTargets int
	if err := pool.QueryRow(
		ctx,
		`SELECT
		     COUNT(*) FILTER (WHERE target_id IN (1, 2, 3) AND status = $2),
		     COUNT(*) FILTER (WHERE target_id = 4 AND status = $3),
		     COUNT(*) FILTER (WHERE target_id = 5 AND status = $4),
		     COUNT(*) FILTER (WHERE target_id = 6 AND status = $5)
		 FROM audit_run_targets
		 WHERE run_id = $1`,
		runID,
		auditTargetStatusAbandoned,
		auditTargetStatusCompleted,
		auditTargetStatusFailed,
		auditTargetStatusCanceled,
	).Scan(&abandonedTargets, &completedTargets, &failedTargets, &canceledTargets); err != nil {
		t.Fatalf("read interrupted stale recovery state: %v", err)
	}
	if abandonedTargets != 3 || completedTargets != 1 || failedTargets != 1 || canceledTargets != 1 {
		t.Fatalf(
			"unexpected interrupted recovery counts: abandoned=%d completed=%d failed=%d canceled=%d",
			abandonedTargets,
			completedTargets,
			failedTargets,
			canceledTargets,
		)
	}
}

func TestStaleRecoveryDefersLockedTargetUntilNextPass(t *testing.T) {
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

	const runID = "fda869a8-5992-46c0-bd29-107876622aa8"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "locked-stale-recovery-worker",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		URLBatchSize:         2,
		DBFetchTimeout:       time.Second,
		DBWriteTimeout:       time.Second,
		StaleRunThreshold:    time.Minute,
		RetryBaseDelay:       time.Millisecond,
		RetryMaxDelay:        5 * time.Millisecond,
	}
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create locked stale audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url, status)
		 VALUES
		     ($1, 1, 'https://locked-stale.example/1', $2),
		     ($1, 2, 'https://locked-stale.example/2', $2)`,
		runID,
		auditTargetStatusPending,
	); err != nil {
		t.Fatalf("insert locked stale targets: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET status = $2,
		     finished_at = CURRENT_TIMESTAMP
		 WHERE id = $1`,
		runID,
		auditRunStatusAbandoned,
	); err != nil {
		t.Fatalf("mark run abandoned before target recovery: %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedTargetID int64
	if err := lockTx.QueryRow(
		ctx,
		`SELECT target_id
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = 1
		 FOR UPDATE`,
		runID,
	).Scan(&lockedTargetID); err != nil {
		t.Fatalf("lock stale target: %v", err)
	}

	if _, err := abandonStaleAuditRuns(ctx, pool, cfg); err != nil {
		t.Fatalf("recover stale targets while one target is locked: %v", err)
	}

	var lockedStatus string
	var recoveredStatus string
	if err := pool.QueryRow(
		ctx,
		`SELECT
		     MAX(status::TEXT) FILTER (WHERE target_id = 1),
		     MAX(status::TEXT) FILTER (WHERE target_id = 2)
		 FROM audit_run_targets
		 WHERE run_id = $1`,
		runID,
	).Scan(&lockedStatus, &recoveredStatus); err != nil {
		t.Fatalf("read partial stale recovery state: %v", err)
	}
	if lockedStatus != auditTargetStatusPending || recoveredStatus != auditTargetStatusAbandoned {
		t.Fatalf(
			"unexpected partial recovery state: locked=%q recovered=%q",
			lockedStatus,
			recoveredStatus,
		)
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release stale target lock: %v", err)
	}
	if _, err := abandonStaleAuditRuns(ctx, pool, cfg); err != nil {
		t.Fatalf("recover deferred stale target on the next pass: %v", err)
	}

	var abandonedTargets int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*)
		 FROM audit_run_targets
		 WHERE run_id = $1 AND status = $2`,
		runID,
		auditTargetStatusAbandoned,
	).Scan(&abandonedTargets); err != nil {
		t.Fatalf("read completed stale recovery state: %v", err)
	}
	if abandonedTargets != 2 {
		t.Fatalf("unexpected recovered target count: got %d, want 2", abandonedTargets)
	}
}

func TestAuditRunClaimsAreExclusiveAndResumable(t *testing.T) {
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

	const runID = "26569014-f160-49aa-92fd-eb4fbd06e297"
	const sourceA = "https://lease-check.example/a"
	const sourceB = "https://lease-check.example/b"
	const sourceC = "https://lease-check.example/late"
	restoreActivePages := suspendActivePages(t, ctx, pool)
	defer restoreActivePages()
	ownerConfig := Config{
		RunID:                runID,
		WorkerInstanceID:     "lease-owner-a",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		DBFetchTimeout:       3 * time.Second,
		TargetLeaseDuration:  2 * time.Minute,
		StaleRunThreshold:    time.Minute,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}
	resumeConfig := ownerConfig
	resumeConfig.WorkerInstanceID = "lease-owner-b"

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM pages_to_scan WHERE url IN ($1, $2, $3)", sourceA, sourceB, sourceC)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if _, err := pool.Exec(
		ctx,
		`INSERT INTO pages_to_scan (url, is_active)
		 VALUES ($1, TRUE), ($2, TRUE)
		 ON CONFLICT (url) DO UPDATE
		 SET is_active = EXCLUDED.is_active`,
		sourceA,
		sourceB,
	); err != nil {
		t.Fatalf("seed lease targets: %v", err)
	}
	if err := createAuditRun(ctx, pool, &ownerConfig); err != nil {
		t.Fatalf("create owned audit run: %v", err)
	}
	if _, err := captureAuditRunTargets(ctx, pool, ownerConfig); err != nil {
		t.Fatalf("capture owned target snapshot: %v", err)
	}
	if err := createAuditRun(ctx, pool, &resumeConfig); err == nil {
		t.Fatal("expected a second parser to be rejected while the audit run owner is active")
	}

	firstClaim, err := claimTargetURLBatch(ctx, pool, ownerConfig, 1)
	if err != nil {
		t.Fatalf("claim first target: %v", err)
	}
	if len(firstClaim) != 1 {
		t.Fatalf("unexpected first claim size: %d", len(firstClaim))
	}

	wrongOwnerTx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin wrong-owner transaction: %v", err)
	}
	err = markAuditRunTargetFinished(
		ctx,
		wrongOwnerTx,
		runID,
		firstClaim[0].ID,
		"intruder",
		effectiveOwnerGeneration(ownerConfig),
		auditTargetStatusCompleted,
		"",
	)
	_ = wrongOwnerTx.Rollback(ctx)
	if err == nil {
		t.Fatal("expected a non-owner to be unable to finish the claimed target")
	}

	if _, err := pool.Exec(
		ctx,
		`INSERT INTO pages_to_scan (url, is_active)
		 VALUES ($1, TRUE)
		 ON CONFLICT (url) DO UPDATE
		 SET is_active = EXCLUDED.is_active`,
		sourceC,
	); err != nil {
		t.Fatalf("insert late target: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET heartbeat_at = CURRENT_TIMESTAMP - INTERVAL '10 minutes'
		 WHERE id = $1`,
		runID,
	); err != nil {
		t.Fatalf("expire audit run heartbeat: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_run_targets
		 SET lease_until = CURRENT_TIMESTAMP - INTERVAL '1 minute'
		 WHERE run_id = $1 AND status = $2`,
		runID,
		auditTargetStatusRunning,
	); err != nil {
		t.Fatalf("expire target lease: %v", err)
	}

	abandoned, err := abandonStaleAuditRuns(ctx, pool, resumeConfig)
	if err != nil {
		t.Fatalf("abandon stale run before resume: %v", err)
	}
	if abandoned < 1 {
		t.Fatalf("expected at least one abandoned run before resume, got %d", abandoned)
	}
	if err := createAuditRun(ctx, pool, &resumeConfig); err != nil {
		t.Fatalf("resume abandoned audit run: %v", err)
	}
	if err := updateAuditRunHeartbeat(ctx, pool, ownerConfig); err == nil {
		t.Fatal("expected the previous owner heartbeat to be rejected after resume")
	}
	resumedSnapshot, err := captureAuditRunTargets(ctx, pool, resumeConfig)
	if err != nil {
		t.Fatalf("read resumed target snapshot: %v", err)
	}
	if resumedSnapshot.Total != 2 {
		t.Fatalf("resume rebuilt the stable snapshot: total=%d want=2", resumedSnapshot.Total)
	}

	type claimResult struct {
		records []targetURLRecord
		err     error
	}
	claims := make(chan claimResult, 2)
	for range 2 {
		go func() {
			records, claimErr := claimTargetURLBatch(ctx, pool, resumeConfig, 1)
			claims <- claimResult{records: records, err: claimErr}
		}()
	}

	claimedIDs := make(map[int64]struct{}, 2)
	for range 2 {
		claim := <-claims
		if claim.err != nil {
			t.Fatalf("claim resumed target: %v", claim.err)
		}
		if len(claim.records) != 1 {
			t.Fatalf("unexpected resumed claim size: %d", len(claim.records))
		}
		claimedIDs[claim.records[0].ID] = struct{}{}
	}
	if len(claimedIDs) != 2 {
		t.Fatalf("concurrent claims returned duplicate targets: %#v", claimedIDs)
	}

	noMoreTargets, err := claimTargetURLBatch(ctx, pool, resumeConfig, 1)
	if err != nil {
		t.Fatalf("check exhausted target claims: %v", err)
	}
	if len(noMoreTargets) != 0 {
		t.Fatalf("expected no unclaimed targets, got %#v", noMoreTargets)
	}
}

func TestResumeWaitsForLockedResumableTarget(t *testing.T) {
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

	const runID = "e75fb89e-5ded-4a34-887d-94ca45242093"
	const requestURL = "https://resume-lock.example/page?token=runtime-secret"
	ownerConfig := Config{
		RunID:                runID,
		WorkerInstanceID:     "resume-lock-old-owner",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBFetchTimeout:       time.Second,
		DBWriteTimeout:       2 * time.Second,
		URLBatchSize:         10,
		DBMaxRetries:         0,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        20 * time.Millisecond,
	}
	resumeConfig := ownerConfig
	resumeConfig.WorkerInstanceID = "resume-lock-new-owner"

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &ownerConfig); err != nil {
		t.Fatalf("create initial audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url, status)
		 VALUES ($1, 1, $2, $3)`,
		runID,
		requestURL,
		auditTargetStatusAbandoned,
	); err != nil {
		t.Fatalf("insert abandoned target: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET status = $2,
		     finished_at = CURRENT_TIMESTAMP
		 WHERE id = $1`,
		runID,
		auditRunStatusAbandoned,
	); err != nil {
		t.Fatalf("mark initial audit run abandoned: %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedTargetID int64
	if err := lockTx.QueryRow(
		ctx,
		`SELECT target_id
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = 1
		 FOR UPDATE`,
		runID,
	).Scan(&lockedTargetID); err != nil {
		t.Fatalf("lock resumable target: %v", err)
	}

	resumeDone := make(chan error, 1)
	go func() {
		resumeDone <- createAuditRun(ctx, pool, &resumeConfig)
	}()
	ownershipDeadline := time.Now().Add(time.Second)
	for {
		var currentWorkerInstanceID string
		if err := pool.QueryRow(
			ctx,
			"SELECT worker_instance_id FROM audit_runs WHERE id = $1",
			runID,
		).Scan(&currentWorkerInstanceID); err != nil {
			t.Fatalf("read audit run owner during resume: %v", err)
		}
		if currentWorkerInstanceID == resumeConfig.WorkerInstanceID {
			break
		}
		if time.Now().After(ownershipDeadline) {
			t.Fatalf("resume did not acquire audit run ownership before contention check")
		}
		select {
		case <-ctx.Done():
			t.Fatalf("resume ownership check was canceled: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case resumeErr := <-resumeDone:
		t.Fatalf("resume returned while resumable target lock was held: %v", resumeErr)
	case <-time.After(100 * time.Millisecond):
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release resumable target lock: %v", err)
	}
	select {
	case resumeErr := <-resumeDone:
		if resumeErr != nil {
			t.Fatalf("resume audit run after lock release: %v", resumeErr)
		}
	case <-ctx.Done():
		t.Fatalf("resume did not finish after target lock release: %v", ctx.Err())
	}

	var runStatus, targetStatus, workerInstanceID string
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status, target.status, run.worker_instance_id
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1 AND target.target_id = 1`,
		runID,
	).Scan(&runStatus, &targetStatus, &workerInstanceID); err != nil {
		t.Fatalf("read resumed audit state: %v", err)
	}
	if runStatus != auditRunStatusRunning ||
		targetStatus != auditTargetStatusPending ||
		workerInstanceID != resumeConfig.WorkerInstanceID {
		t.Fatalf(
			"resume left inconsistent state: run=%q target=%q worker=%q",
			runStatus,
			targetStatus,
			workerInstanceID,
		)
	}
}

func TestRepeatedWorkerIDCannotPersistAfterOwnershipTakeover(t *testing.T) {
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

	const runID = "0fa05d62-1a7d-4c7d-a209-b4c0e33d47f5"
	const sourceURL = "https://generation-fence.example/page?token=runtime-secret"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "reused-worker-id",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		DBFetchTimeout:       3 * time.Second,
		TargetLeaseDuration:  2 * time.Minute,
		StaleRunThreshold:    time.Minute,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}
	staleOwner := cfg
	freshOwner := cfg

	restoreActivePages := suspendActivePages(t, ctx, pool)
	defer restoreActivePages()
	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM pages_to_scan WHERE url = $1", sourceURL)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if _, err := pool.Exec(
		ctx,
		`INSERT INTO pages_to_scan (url, is_active) VALUES ($1, TRUE)`,
		sourceURL,
	); err != nil {
		t.Fatalf("seed generation-fence target: %v", err)
	}
	if err := createAuditRun(ctx, pool, &staleOwner); err != nil {
		t.Fatalf("create first owner generation: %v", err)
	}
	if _, err := captureAuditRunTargets(ctx, pool, staleOwner); err != nil {
		t.Fatalf("capture generation-fence snapshot: %v", err)
	}
	firstClaim, err := claimTargetURLBatch(ctx, pool, staleOwner, 1)
	if err != nil || len(firstClaim) != 1 {
		t.Fatalf("claim target with first generation: records=%#v error=%v", firstClaim, err)
	}

	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_runs
		 SET heartbeat_at = CURRENT_TIMESTAMP - INTERVAL '10 minutes'
		 WHERE id = $1`,
		runID,
	); err != nil {
		t.Fatalf("expire first owner heartbeat: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE audit_run_targets
		 SET lease_until = CURRENT_TIMESTAMP - INTERVAL '1 minute'
		 WHERE run_id = $1 AND target_id = $2`,
		runID,
		firstClaim[0].ID,
	); err != nil {
		t.Fatalf("expire first owner target lease: %v", err)
	}
	if _, err := abandonStaleAuditRuns(ctx, pool, freshOwner); err != nil {
		t.Fatalf("abandon stale owner generation: %v", err)
	}
	if err := createAuditRun(ctx, pool, &freshOwner); err != nil {
		t.Fatalf("acquire replacement owner generation: %v", err)
	}
	if freshOwner.OwnerGeneration <= staleOwner.OwnerGeneration {
		t.Fatalf(
			"ownership generation did not advance: stale=%d fresh=%d",
			staleOwner.OwnerGeneration,
			freshOwner.OwnerGeneration,
		)
	}
	freshClaim, err := claimTargetURLBatch(ctx, pool, freshOwner, 1)
	if err != nil || len(freshClaim) != 1 || freshClaim[0].ID != firstClaim[0].ID {
		t.Fatalf("claim target with replacement generation: records=%#v error=%v", freshClaim, err)
	}

	target := newAuditTarget(firstClaim[0], firstClaim[0].URL, staleOwner.TargetFingerprintKey)
	staleResults := make(chan Result, 1)
	staleResults <- Result{Target: target, Data: SEOData{
		URL:         sourceURL,
		ScanStatus:  scanStatusCompleted,
		Title:       "stale owner result",
		TitleStatus: "OK",
	}}
	close(staleResults)
	staleSummary := saveResults(ctx, pool, staleResults, staleOwner)
	if staleSummary.PersistenceFailures != 1 || staleSummary.Saved != 0 {
		t.Fatalf("stale owner result was not rejected: %+v", staleSummary)
	}

	var resultCount int
	if err := pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM audit_results WHERE run_id = $1 AND target_id = $2",
		runID,
		target.TargetID,
	).Scan(&resultCount); err != nil {
		t.Fatalf("count stale owner results: %v", err)
	}
	if resultCount != 0 {
		t.Fatalf("stale owner transaction became durable: result_count=%d", resultCount)
	}

	freshTarget := newAuditTarget(freshClaim[0], freshClaim[0].URL, freshOwner.TargetFingerprintKey)
	freshResults := make(chan Result, 1)
	freshResults <- Result{Target: freshTarget, Data: SEOData{
		URL:         sourceURL,
		ScanStatus:  scanStatusCompleted,
		Title:       "fresh owner result",
		TitleStatus: "OK",
	}}
	close(freshResults)
	freshSummary := saveResults(ctx, pool, freshResults, freshOwner)
	if freshSummary.PersistenceFailures != 0 || freshSummary.Saved != 1 {
		t.Fatalf("replacement owner could not persist result: %+v", freshSummary)
	}

	var storedTitle string
	if err := pool.QueryRow(
		ctx,
		"SELECT title FROM audit_results WHERE run_id = $1 AND target_id = $2",
		runID,
		target.TargetID,
	).Scan(&storedTitle); err != nil {
		t.Fatalf("read replacement owner result: %v", err)
	}
	if storedTitle != "fresh owner result" {
		t.Fatalf("unexpected stored result title: %q", storedTitle)
	}
}

func TestHeartbeatMonitorCancelsWorkAfterOwnershipLoss(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	applyIntegrationMigrations(t, ctx, databaseURL)

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create PostgreSQL pool: %v", err)
	}
	defer pool.Close()

	const runID = "979c693c-8431-4ee6-9ea8-57b98589ac76"
	cfg := Config{
		RunID:                     runID,
		WorkerInstanceID:          "heartbeat-owner",
		DBWriteTimeout:            time.Second,
		AuditRunHeartbeatInterval: 10 * time.Millisecond,
		HeartbeatFailureThreshold: 2,
		TargetLeaseDuration:       time.Minute,
		DBMaxRetries:              0,
		RetryBaseDelay:            time.Millisecond,
		RetryMaxDelay:             2 * time.Millisecond,
	}
	_, _ = pool.Exec(ctx, "DELETE FROM audit_runs WHERE id = $1", runID)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create heartbeat test run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		"UPDATE audit_runs SET worker_instance_id = 'replacement-owner' WHERE id = $1",
		runID,
	); err != nil {
		t.Fatalf("replace audit run owner: %v", err)
	}

	workCtx, cancelWork := context.WithCancelCause(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	done := startAuditRunHeartbeat(heartbeatCtx, pool, cfg, cancelWork)
	defer func() {
		stopHeartbeat()
		<-done
	}()

	select {
	case <-workCtx.Done():
		cause := context.Cause(workCtx)
		if cause == nil || !strings.Contains(cause.Error(), "heartbeat failed 2 consecutive times") {
			t.Fatalf("unexpected heartbeat cancellation cause: %v", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat ownership loss did not cancel work scheduling")
	}
}

func TestFailedAuditRunPreservesTargetForResume(t *testing.T) {
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

	const runID = "f8cd2ba6-e2e9-4436-89e0-56d0611a098f"
	const requestURL = "https://resume-check.example/page?token=runtime-secret"
	ownerConfig := Config{
		RunID:                runID,
		WorkerInstanceID:     "failed-run-owner",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		DBFetchTimeout:       3 * time.Second,
		TargetLeaseDuration:  2 * time.Minute,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}
	resumeConfig := ownerConfig
	resumeConfig.WorkerInstanceID = "failed-run-resumer"

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &ownerConfig); err != nil {
		t.Fatalf("create audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url)
		 VALUES ($1, 1, $2)`,
		runID,
		requestURL,
	); err != nil {
		t.Fatalf("insert audit target: %v", err)
	}
	claimed, err := claimTargetURLBatch(ctx, pool, ownerConfig, 1)
	if err != nil {
		t.Fatalf("claim audit target: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("unexpected claimed target count: %d", len(claimed))
	}

	if err := completeAuditRun(ctx, pool, runID, auditRunCompletion{
		Status:     auditRunStatusFailed,
		TotalURLs:  1,
		FailedURLs: 1,
	}, ownerConfig); err != nil {
		t.Fatalf("fail audit run: %v", err)
	}

	var runStatus, targetStatus, retainedURL string
	var claimedBy *string
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status, target.status, target.request_url, target.claimed_by
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1 AND target.target_id = 1`,
		runID,
	).Scan(&runStatus, &targetStatus, &retainedURL, &claimedBy); err != nil {
		t.Fatalf("read failed run state: %v", err)
	}
	if runStatus != auditRunStatusFailed ||
		targetStatus != auditTargetStatusPending ||
		retainedURL != requestURL ||
		claimedBy != nil {
		t.Fatalf(
			"failed run did not preserve resumable target: run=%q target=%q url=%q claimed_by=%v",
			runStatus,
			targetStatus,
			retainedURL,
			claimedBy,
		)
	}

	if err := createAuditRun(ctx, pool, &resumeConfig); err != nil {
		t.Fatalf("resume failed audit run: %v", err)
	}
	resumed, err := claimTargetURLBatch(ctx, pool, resumeConfig, 1)
	if err != nil {
		t.Fatalf("claim resumed target: %v", err)
	}
	if len(resumed) != 1 || resumed[0].ID != 1 || resumed[0].URL != requestURL {
		t.Fatalf("unexpected resumed target: %#v", resumed)
	}
}

func TestCanceledCompletionWaitsForPostgreSQLTargetLock(t *testing.T) {
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

	const runID = "a401aa1e-b973-48ea-a256-f9713aa15c71"
	const requestURL = "https://completion-lock.example/page?token=runtime-secret"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "completion-lock-owner",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBFetchTimeout:       time.Second,
		DBWriteTimeout:       2 * time.Second,
		URLBatchSize:         10,
		DBMaxRetries:         0,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        20 * time.Millisecond,
	}

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (
		     run_id, target_id, request_url, status, claimed_by, claimed_at, lease_until
		 ) VALUES ($1, 1, $2, $3, $4, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + INTERVAL '2 minutes')`,
		runID,
		requestURL,
		auditTargetStatusRunning,
		cfg.WorkerInstanceID,
	); err != nil {
		t.Fatalf("insert running audit target: %v", err)
	}
	if err := finalizeAuditRun(ctx, pool, runID, auditRunCompletion{
		Status:    auditRunStatusCanceled,
		TotalURLs: 1,
	}, cfg); err == nil {
		t.Fatal("terminal update accepted a canceled run with a running target")
	}
	var statusBeforeCompletion string
	if err := pool.QueryRow(ctx, "SELECT status FROM audit_runs WHERE id = $1", runID).Scan(&statusBeforeCompletion); err != nil {
		t.Fatalf("read audit run after rejected terminal update: %v", err)
	}
	if statusBeforeCompletion != auditRunStatusRunning {
		t.Fatalf("rejected terminal update changed run status to %q", statusBeforeCompletion)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedTargetID int64
	if err := lockTx.QueryRow(
		ctx,
		`SELECT target_id
		 FROM audit_run_targets
		 WHERE run_id = $1 AND target_id = 1
		 FOR UPDATE`,
		runID,
	).Scan(&lockedTargetID); err != nil {
		t.Fatalf("lock running audit target: %v", err)
	}

	completionDone := make(chan error, 1)
	go func() {
		completionDone <- completeAuditRun(ctx, pool, runID, auditRunCompletion{
			Status:    auditRunStatusCanceled,
			TotalURLs: 1,
		}, cfg)
	}()

	select {
	case completionErr := <-completionDone:
		t.Fatalf("completion returned while target lock was held: %v", completionErr)
	case <-time.After(100 * time.Millisecond):
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release target lock: %v", err)
	}
	select {
	case completionErr := <-completionDone:
		if completionErr != nil {
			t.Fatalf("complete canceled audit run after lock release: %v", completionErr)
		}
	case <-ctx.Done():
		t.Fatalf("completion did not finish after target lock release: %v", ctx.Err())
	}

	var runStatus, targetStatus, retainedURL string
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status, target.status, target.request_url
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1 AND target.target_id = 1`,
		runID,
	).Scan(&runStatus, &targetStatus, &retainedURL); err != nil {
		t.Fatalf("read canceled run state: %v", err)
	}
	if runStatus != auditRunStatusCanceled || targetStatus != auditTargetStatusCanceled || retainedURL != "" {
		t.Fatalf(
			"canceled completion left inconsistent state: run=%q target=%q request_url=%q",
			runStatus,
			targetStatus,
			retainedURL,
		)
	}
}

func TestCompletedRunRejectsNonTerminalTargets(t *testing.T) {
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

	const runID = "202bc44a-b871-485d-b749-571361c058de"
	const requestURL = "https://completion-check.example/page"
	cfg := Config{
		RunID:                runID,
		WorkerInstanceID:     "completion-owner",
		TargetFingerprintKey: []byte("local-development-only-fingerprint-key"),
		DBWriteTimeout:       3 * time.Second,
		DBMaxRetries:         2,
		RetryBaseDelay:       10 * time.Millisecond,
		RetryMaxDelay:        50 * time.Millisecond,
	}

	cleanup := func(cleanupCtx context.Context) {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM audit_runs WHERE id = $1", runID)
	}
	cleanup(ctx)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup(cleanupCtx)
	}()

	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatalf("create audit run: %v", err)
	}
	if _, err := pool.Exec(
		ctx,
		`INSERT INTO audit_run_targets (run_id, target_id, request_url)
		 VALUES ($1, 1, $2)`,
		runID,
		requestURL,
	); err != nil {
		t.Fatalf("insert pending audit target: %v", err)
	}

	err = completeAuditRun(ctx, pool, runID, auditRunCompletion{
		Status:     auditRunStatusCompletedWithErrors,
		TotalURLs:  1,
		FailedURLs: 1,
	}, cfg)
	if err == nil {
		t.Fatal("expected completion with a non-terminal target to fail")
	}

	var runStatus, targetStatus, retainedURL string
	if err := pool.QueryRow(
		ctx,
		`SELECT run.status, target.status, target.request_url
		 FROM audit_runs AS run
		 JOIN audit_run_targets AS target ON target.run_id = run.id
		 WHERE run.id = $1 AND target.target_id = 1`,
		runID,
	).Scan(&runStatus, &targetStatus, &retainedURL); err != nil {
		t.Fatalf("read rolled-back completion state: %v", err)
	}
	if runStatus != auditRunStatusRunning ||
		targetStatus != auditTargetStatusPending ||
		retainedURL != requestURL {
		t.Fatalf(
			"failed completion changed durable state: run=%q target=%q url=%q",
			runStatus,
			targetStatus,
			retainedURL,
		)
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
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = adminDB.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+quoteSQLIdentifier(schemaName)+` CASCADE`)
	}()

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
	const legacyURLPrefix = "https://example.com/report/"
	const legacyURLSuffix = "?token=real-secret&view=full#secret-fragment"
	legacyURL := legacyURLPrefix +
		strings.Repeat("a", 2048-len(legacyURLPrefix)-len(legacyURLSuffix)) +
		legacyURLSuffix
	if len(legacyURL) != 2048 {
		t.Fatalf("legacy URL length = %d, want 2048", len(legacyURL))
	}
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
	if version, err := goose.GetDBVersionContext(ctx, migrationDB); err != nil {
		t.Fatalf("read upgraded schema version: %v", err)
	} else if version != requiredSchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, requiredSchemaVersion)
	}

	var targetID int64
	var title string
	var fingerprintKeyID string
	var safeURL string
	if err := migrationDB.QueryRowContext(
		ctx,
		`SELECT target_id, title, fingerprint_key_id, safe_url
		 FROM audit_results
		 WHERE run_id = $1::UUID`,
		runID,
	).Scan(&targetID, &title, &fingerprintKeyID, &safeURL); err != nil {
		t.Fatalf("read upgraded audit result: %v", err)
	}
	if !strings.HasPrefix(safeURL, "https://redacted.invalid/legacy/audit_results/") {
		t.Fatalf("legacy URL was not replaced with a safe identity: %q", safeURL)
	}
	if strings.Contains(safeURL, "real-secret") || len(safeURL) > 2048 {
		t.Fatalf("legacy safe URL is unsafe or oversized: %q", safeURL)
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

func suspendActivePages(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()

	rows, err := pool.Query(ctx, `SELECT id FROM pages_to_scan WHERE is_active = TRUE`)
	if err != nil {
		t.Fatalf("read active pages before isolated test: %v", err)
	}
	var activeIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan active page before isolated test: %v", err)
		}
		activeIDs = append(activeIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate active pages before isolated test: %v", err)
	}
	rows.Close()

	if _, err := pool.Exec(ctx, `UPDATE pages_to_scan SET is_active = FALSE WHERE is_active = TRUE`); err != nil {
		t.Fatalf("suspend active pages for isolated test: %v", err)
	}

	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, id := range activeIDs {
			_, _ = pool.Exec(cleanupCtx, `UPDATE pages_to_scan SET is_active = TRUE WHERE id = $1`, id)
		}
	}
}
