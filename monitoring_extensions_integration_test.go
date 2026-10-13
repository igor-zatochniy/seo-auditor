//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func TestAdaptiveConditionalAuditLifecycle(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 90*time.Second)
	var phase, conditional atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if phase.Load() >= 3 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"revision-1"`)
		if phase.Load() == 2 {
			w.Header().Set("X-Robots-Tag", "googlebot: noindex")
		}
		if r.Header.Get("If-None-Match") == `"revision-1"` {
			conditional.Add(1)
			w.WriteHeader(304)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head><title>Stable page</title><link rel="canonical" href="https://example.com/canonical?token=canonical-fixture"><link rel="alternate" hreflang="es" href="https://example.com/es?token=alternate-fixture"><script type="application/ld+json">{"@type":"Product"}</script></head><body><h1>Stable heading</h1>Visible words</body></html>`)
	}))
	defer server.Close()
	m := newAuditManager(ctx, pool, cfg)
	m.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	s := &webServer{pool: pool, cfg: cfg, manager: m}
	input := scheduleInput{ID: newWebRunID(), Name: "Адаптивний тест", URLs: server.URL + "/page?token=request-fixture", Mode: "list", Adaptive: true, IntervalHours: 1, MaxIntervalHours: 24, FailureThreshold: 2, NextRunAt: time.Now().Add(-time.Second)}
	source, label, count, err := prepareSchedule(&input, cfg, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.storeSchedule(ctx, input, source, label, count); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.shutdown()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanup, `DELETE FROM audit_schedules WHERE id=$1`, input.ID)
	})
	start := func() string {
		t.Helper()
		id, err := m.startScheduled(ctx, input.ID)
		if err != nil {
			t.Fatal(err)
		}
		waitScheduled(t, m)
		if done, err := s.compareScheduledRun(ctx); err != nil || !done {
			t.Fatalf("compare: %v %v", done, err)
		}
		return id
	}
	first := start()
	phase.Store(1)
	second := start()
	var status, response int
	var reused, firstCanonical, secondCanonical string
	var issues int
	var snapshot string
	if err = pool.QueryRow(ctx, `SELECT status_code,http_response_status,reused_from_run_id::TEXT,jsonb_array_length(structured_data->'issues'),international->>'canonical_fingerprint' FROM audit_results WHERE run_id=$1`, second).Scan(&status, &response, &reused, &issues, &secondCanonical); err != nil {
		t.Fatal(err)
	}
	if status != 200 || response != 304 || reused != first || issues < 1 || conditional.Load() != 1 {
		t.Fatalf("invalid reused result: status=%d response=%d reused=%s issues=%d conditional=%d", status, response, reused, issues, conditional.Load())
	}
	if err = pool.QueryRow(ctx, `SELECT international->>'canonical_fingerprint' FROM audit_results WHERE run_id=$1`, first).Scan(&firstCanonical); err != nil {
		t.Fatal(err)
	}
	if firstCanonical == "" || firstCanonical != secondCanonical {
		t.Fatal("canonical HMAC changed during 304 reuse")
	}
	if err = pool.QueryRow(ctx, `SELECT data::TEXT FROM audit_http_snapshots WHERE run_id=$1`, second).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"request-fixture", "canonical-fixture", "alternate-fixture"} {
		if strings.Contains(snapshot, secret) {
			t.Fatal("snapshot contains a raw secret")
		}
	}
	phase.Store(2)
	third := start()
	var eventCount int
	if err = pool.QueryRow(ctx, `SELECT http_response_status FROM audit_results WHERE run_id=$1`, third).Scan(&response); err != nil {
		t.Fatal(err)
	}
	if response != 200 {
		t.Fatal("changed robots header was accepted from stale 304 metadata")
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND kind='noindex'`, third).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("noindex event: %d %v", eventCount, err)
	}
	phase.Store(3)
	fourth := start()
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND severity='critical'`, fourth).Scan(&eventCount); err != nil || eventCount != 0 {
		t.Fatalf("transient error not suppressed: %d %v", eventCount, err)
	}
	fifth := start()
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND kind='repeated_failure'`, fifth).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("confirmed failure missing: %d %v", eventCount, err)
	}
	if done, err := s.compareScheduledRun(ctx); err != nil || done {
		t.Fatalf("repeated comparison was not idempotent: %v %v", done, err)
	}
}

func TestBotImportIdempotencyAndURLIsolation(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	s := &webServer{pool: pool, cfg: cfg, submissions: make(chan struct{}, 1)}
	id := newWebRunID()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanup, `DELETE FROM audit_bot_imports WHERE id=$1`, id)
	})
	log := `{"time":"2026-10-12T09:00:00Z","remote_addr":"66.249.66.1","request":"GET /report?token=first-private-fixture HTTP/1.1","user_agent":"Googlebot/2.1","status":200}` + "\n" +
		`{"time":"2026-10-12T09:00:01Z","remote_addr":"66.249.66.1","request":"GET /report?token=second-private-fixture HTTP/1.1","user_agent":"Googlebot/2.1","status":404}` + "\n" +
		`{"time":"2026-10-12T09:00:02Z","remote_addr":"66.249.66.1","request":"GET https://other.example/private HTTP/1.1","user_agent":"Googlebot/2.1","status":200}`
	call := func(importID string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"id": importID, "domain": "example.com", "format": "jsonl", "log": log})
		r := httptest.NewRequest("POST", "http://localhost/api/bots/imports", strings.NewReader(string(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		s.importBotLogs(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("import HTTP %d: %s", w.Code, w.Body.String())
		}
		return w.Code
	}
	if call(id) != 201 || call(id) != 200 || call(newWebRunID()) != 200 {
		t.Fatal("import idempotency failed")
	}
	var count, distinct int
	var raw string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(DISTINCT target_fingerprint),string_agg(safe_url,' ') FROM audit_bot_events WHERE import_id=$1`, id).Scan(&count, &distinct, &raw); err != nil {
		t.Fatal(err)
	}
	if count != 2 || distinct != 2 || strings.Contains(raw, "private-fixture") || strings.Contains(raw, "other.example") {
		t.Fatalf("URL isolation/redaction failed: %d %d %s", count, distinct, raw)
	}
	r := httptest.NewRequest("GET", "http://localhost/api/bots/imports/"+id, nil).WithContext(ctx)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.botImportReport(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "66.249.66.1") || strings.Contains(w.Body.String(), "private-fixture") {
		t.Fatalf("unsafe report: %d %s", w.Code, w.Body.String())
	}
}

func TestAdaptiveAuditUsesBoundedSnapshots(t *testing.T) {
	ctx, pool, cfg := webIntegration(t, 90*time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head><title>Batch page</title></head><body><h1>Batch heading</h1></body></html>")
	}))
	defer server.Close()
	urls := []string{}
	for i := 0; i < adaptiveBatchSize+1; i++ {
		urls = append(urls, server.URL+"/page/"+fmt.Sprint(i))
	}
	m := newAuditManager(ctx, pool, cfg)
	m.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	s := &webServer{pool: pool, cfg: cfg, manager: m}
	input := scheduleInput{ID: newWebRunID(), Name: "Пакетний тест", URLs: strings.Join(urls, "\n"), Adaptive: true, IntervalHours: 1, MaxIntervalHours: 24, NextRunAt: time.Now().Add(-time.Second)}
	source, label, count, err := prepareSchedule(&input, cfg, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.storeSchedule(ctx, input, source, label, count); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.shutdown()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanup, `DELETE FROM audit_schedules WHERE id=$1`, input.ID)
	})
	for _, expected := range []int{adaptiveBatchSize, 1} {
		id, err := m.startScheduled(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		waitScheduled(t, m)
		var total int
		if err = pool.QueryRow(ctx, `SELECT total_urls FROM audit_runs WHERE id=$1`, id).Scan(&total); err != nil || total != expected {
			t.Fatalf("unbounded snapshot: %d %v", total, err)
		}
		if done, err := s.compareScheduledRun(ctx); err != nil || !done {
			t.Fatalf("compare: %v %v", done, err)
		}
	}
	if _, err = m.startScheduled(ctx, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("targets ran before due time: %v", err)
	}
}

func TestMonitoringExtensionsUpgradePreservesExistingReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || dsn.Host == "" {
		t.Fatal("DATABASE_URL is required")
	}
	admin, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "upgrade34_" + strings.ReplaceAll(newWebRunID(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`)
	})
	q := dsn.Query()
	q.Set("search_path", schema)
	dsn.RawQuery = q.Encode()
	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	goose.SetBaseFS(migrationFiles)
	goose.SetTableName(migrationVersionTable)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("pgx"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, db, migrationDir, 31); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := Config{RunID: newWebRunID(), WorkerInstanceID: "upgrade34", TargetFingerprintKey: []byte("local-development-only-fingerprint-key"), TargetFingerprintKeyID: "default", DBWriteTimeout: 3 * time.Second, DBFetchTimeout: 5 * time.Second, OwnerGeneration: 1}
	if err = createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,title) VALUES($1,1,'https://example.com/',$2,'default',200,'Existing SEO report')`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/")); err != nil {
		t.Fatal(err)
	}
	geoID := newWebRunID()
	if _, err = pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count) VALUES($1,$2,$3,'example.com','local-lexical-v4',1,1,1,0)`, geoID, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "fixture")); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrationsDB(ctx, db); err != nil {
		t.Fatal(err)
	}
	var title string
	var total int
	if err = pool.QueryRow(ctx, `SELECT title FROM audit_results WHERE run_id=$1`, cfg.RunID).Scan(&title); err != nil || title != "Existing SEO report" {
		t.Fatalf("SEO data lost: %q %v", title, err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM geo_reports WHERE id=$1`, geoID).Scan(&total); err != nil || total != 1 {
		t.Fatalf("GEO data lost: %d %v", total, err)
	}
	if err = applySchemaMigrationsDB(ctx, db); err != nil {
		t.Fatalf("repeated migration is not idempotent: %v", err)
	}
}
