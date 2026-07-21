//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditPipelinePersistsResult(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
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
	persistPipelineResult := func(cfg Config) ResultSummary {
		t.Helper()
		jobs := make(chan string, 1)
		jobs <- targetURL
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
	firstSummary := persistPipelineResult(firstConfig)
	if firstSummary.Saved != 1 || firstSummary.Successful != 1 || firstSummary.Failed != 0 {
		t.Fatalf("unexpected first save summary: %+v", firstSummary)
	}

	updatedResults := make(chan Result, 1)
	updatedResults <- Result{Data: SEOData{
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
	secondSummary := persistPipelineResult(secondConfig)
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
	signedResults := make(chan Result, 2)
	signedResults <- Result{Data: SEOData{
		URL:           targetURL + "?token=AAAA",
		StatusCode:    httpStatus(http.StatusOK),
		ScanStatus:    scanStatusCompleted,
		Title:         "Signed URL A",
		RobotsAllowed: true,
		RobotsOutcome: robotsOutcomeAllowed,
	}}
	signedResults <- Result{Data: SEOData{
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
		RunID:          runID,
		DBWriteTimeout: 3 * time.Second,
		DBFetchTimeout: 3 * time.Second,
		DBMaxRetries:   2,
		RetryBaseDelay: 10 * time.Millisecond,
		RetryMaxDelay:  50 * time.Millisecond,
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

	jobs := make(chan string, 4)
	streamSummary := streamTargetURLs(
		ctx,
		snapshot.HighWatermark,
		2,
		false,
		jobs,
		func(batchCtx context.Context, afterID, highWatermark int64, limit int) ([]targetURLRecord, error) {
			return fetchTargetURLBatch(batchCtx, pool, cfg, afterID, highWatermark, limit)
		},
	)
	close(jobs)

	if streamSummary.Error != nil {
		t.Fatalf("stream stable targets: %v", streamSummary.Error)
	}
	if streamSummary.Queued != 2 || streamSummary.Skipped != 0 {
		t.Fatalf("unexpected stream summary: %+v", streamSummary)
	}

	got := make(map[string]struct{})
	for targetURL := range jobs {
		got[targetURL] = struct{}{}
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
