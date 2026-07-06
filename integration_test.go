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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	const runID = "integration-test-run"
	jobs := make(chan string, 1)
	jobs <- server.URL + "/page"
	close(jobs)
	results := make(chan Result, 1)
	cfg := Config{
		RunID:              runID,
		HTTPRequestTimeout: 2 * time.Second,
		RobotsTimeout:      time.Second,
		DBWriteTimeout:     3 * time.Second,
		MaxHTMLBodyBytes:   DefaultMaxHTMLBodyBytes,
		DBMaxRetries:       2,
		RetryBaseDelay:     10 * time.Millisecond,
		RetryMaxDelay:      50 * time.Millisecond,
	}

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

	summary := saveResults(ctx, pool, results, cfg)
	if summary.Saved != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected save summary: %+v", summary)
	}

	var statusCode int
	var scanStatus, storedRunID, title string
	err = pool.QueryRow(
		ctx,
		"SELECT status_code, scan_status, run_id, title FROM seo_results WHERE url = $1",
		server.URL+"/page",
	).Scan(&statusCode, &scanStatus, &storedRunID, &title)
	if err != nil {
		t.Fatalf("read persisted audit result: %v", err)
	}
	if statusCode != http.StatusOK || scanStatus != scanStatusCompleted || storedRunID != runID {
		t.Fatalf("unexpected persisted outcome: status=%d scan=%q run=%q", statusCode, scanStatus, storedRunID)
	}
	if title != "Integration audit result page title" {
		t.Fatalf("unexpected persisted title: %q", title)
	}
}
