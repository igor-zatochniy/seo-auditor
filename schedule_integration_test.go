//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/igor-zatochniy/seo-auditor/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationSchedule(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg Config, urls string) (*webServer, string) {
	t.Helper()
	m := newAuditManager(ctx, pool, cfg)
	m.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	s := &webServer{pool: pool, cfg: cfg, manager: m}
	input := scheduleInput{ID: newWebRunID(), Name: "Тест моніторингу", URLs: urls, IntervalHours: 24, NextRunAt: time.Now().Add(-30 * time.Second)}
	source, label, count, err := prepareSchedule(&input, cfg, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.storeSchedule(ctx, input, source, label, count); err != nil {
		t.Fatal(err)
	}
	if err = s.storeSchedule(ctx, input, source, label, count); err != nil {
		t.Fatalf("idempotent submission: %v", err)
	}
	t.Cleanup(func() {
		m.shutdown()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanup, `DELETE FROM audit_schedules WHERE id=$1`, input.ID)
	})
	return s, input.ID
}

func waitScheduled(t *testing.T, m *AuditManager) {
	t.Helper()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled audit did not finish")
	}
}

func TestScheduledAuditMonitoringLifecycle(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var phase atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if phase.Load() > 0 && r.URL.Query().Get("token") == "second-secret" {
			http.NotFound(w, r)
			return
		}
		if phase.Load() == 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		rules := "index"
		canonical := "/old"
		if phase.Load() > 0 {
			rules = "noindex"
			canonical = "/new"
		}
		_, _ = io.WriteString(w, `<html><head><title>Test</title><meta name="robots" content="`+rules+`"><link rel="canonical" href="`+canonical+`"></head><body><h1>Page</h1>content</body></html>`)
	}))
	defer server.Close()
	s, id := integrationSchedule(t, ctx, pool, cfg, server.URL+"/page?token=first-secret\n"+server.URL+"/page?token=second-secret")
	first, err := s.manager.startScheduled(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	if ok, err := s.compareScheduledRun(ctx); err != nil || !ok {
		t.Fatalf("baseline ok=%v err=%v", ok, err)
	}
	phase.Store(1)
	second, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	if ok, err := s.compareScheduledRun(ctx); err != nil || !ok {
		t.Fatalf("compare ok=%v err=%v", ok, err)
	}
	var baseline string
	var count int
	if err = pool.QueryRow(ctx, `SELECT baseline_run_id::TEXT,change_count FROM audit_schedule_runs WHERE run_id=$1`, second).Scan(&baseline, &count); err != nil || baseline != first || count != 3 {
		t.Fatalf("baseline=%s count=%d err=%v", baseline, count, err)
	}
	var kinds []string
	var leaked bool
	if err = pool.QueryRow(ctx, `SELECT array_agg(kind ORDER BY kind),BOOL_OR(safe_url LIKE '%first-secret%' OR safe_url LIKE '%second-secret%') FROM audit_change_events WHERE run_id=$1`, second).Scan(&kinds, &leaked); err != nil || leaked || strings.Join(kinds, ",") != "canonical_changed,http_error,noindex" {
		t.Fatalf("kinds=%v leaked=%v err=%v", kinds, leaked, err)
	}
	if ok, err := s.compareScheduledRun(ctx); err != nil || ok {
		t.Fatalf("duplicate comparison ok=%v err=%v", ok, err)
	}
	third, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	if _, err = s.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT change_count FROM audit_schedule_runs WHERE run_id=$1`, third).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unchanged page raised %d alerts: %v", count, err)
	}
	phase.Store(2)
	fourth, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	if _, err = s.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND kind='request_failed'`, fourth).Scan(&count); err != nil || count != 1 {
		t.Fatalf("HTML replaced by JSON was not reported: %d %v", count, err)
	}
	if run, err := loadWebRun(ctx, pool, cfg, second); err != nil || run.Resumable {
		t.Fatalf("scheduled history must remain immutable: %+v %v", run, err)
	}
}

func TestScheduledClaimsAreExclusiveAcrossManagers(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	s, id := integrationSchedule(t, ctx, pool, cfg, "https://example.com/")
	other := newAuditManager(ctx, pool, cfg)
	t.Cleanup(other.shutdown)
	blocked := func(ctx context.Context, _ *pgxpool.Pool, _ Config, _ bool) int { <-ctx.Done(); return exitCanceled }
	s.manager.execute = blocked
	other.execute = blocked
	var wg sync.WaitGroup
	var successes atomic.Int32
	for _, m := range []*AuditManager{s.manager, other} {
		wg.Add(1)
		go func(m *AuditManager) {
			defer wg.Done()
			_, err := m.startScheduled(ctx, id)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("claim: %v", err)
			}
		}(m)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("%d duplicate launches", successes.Load())
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_schedule_runs WHERE schedule_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs=%d err=%v", count, err)
	}
}

func TestScheduledRestartRecoveryAndKeyRotation(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	s, id := integrationSchedule(t, ctx, pool, cfg, "https://example.com/")
	s.manager.execute = func(_ context.Context, pool *pgxpool.Pool, c Config, _ bool) int {
		_, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=CURRENT_TIMESTAMP WHERE id=$1`, c.RunID)
		if err != nil {
			t.Error(err)
		}
		return exitSuccess
	}
	first, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	// Новий об'єкт сервісу читає незавершене порівняння з PostgreSQL.
	restarted := &webServer{pool: pool, cfg: cfg, manager: s.manager}
	if ok, err := restarted.compareScheduledRun(ctx); err != nil || !ok {
		t.Fatalf("recovery %v %v", ok, err)
	}
	s.manager.cfg.TargetFingerprintKey = []byte("another-development-only-key-for-tests")
	second, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	if _, err = restarted.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	var comparison string
	var baseline *string
	if err = pool.QueryRow(ctx, `SELECT comparison_status,baseline_run_id::TEXT FROM audit_schedule_runs WHERE run_id=$1`, second).Scan(&comparison, &baseline); err != nil || comparison != "baseline" || baseline != nil {
		t.Fatalf("key rotation comparison=%s baseline=%v err=%v", comparison, baseline, err)
	}
	_, _ = pool.Exec(ctx, `UPDATE audit_runs SET status='abandoned' WHERE id=$1`, first)
	_, _ = pool.Exec(ctx, `UPDATE audit_schedule_runs SET comparison_status='pending' WHERE run_id=$1`, first)
	if _, err = restarted.compareScheduledRun(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND kind='audit_interrupted'`, first).Scan(&count); err != nil || count != 1 {
		t.Fatalf("interruption events=%d err=%v", count, err)
	}
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1 AND request_url<>''`, first).Scan(&count); err != nil || count != 0 {
		t.Fatalf("interrupted snapshot retained URLs: %d %v", count, err)
	}
}

func TestInvalidSchedulePausesWithoutBlockingOtherSchedules(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	s, id := integrationSchedule(t, ctx, pool, cfg, "http://127.0.0.1/private")
	s.manager.cfg.AllowPrivateTargets = false
	if _, err := s.manager.startScheduled(ctx, ""); err == nil {
		t.Fatal("unsafe schedule launched")
	}
	var enabled bool
	var message string
	if err := pool.QueryRow(ctx, `SELECT enabled,last_error FROM audit_schedules WHERE id=$1`, id).Scan(&enabled, &message); err != nil || enabled || message == "" {
		t.Fatalf("enabled=%v message=%q err=%v", enabled, message, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_schedule_runs WHERE schedule_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial run committed: %d %v", count, err)
	}
}

func TestScheduledNotificationPaginationAndReadState(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	var urls []string
	for i := range 105 {
		urls = append(urls, fmt.Sprintf("https://example.com/page/%d", i))
	}
	s, id := integrationSchedule(t, ctx, pool, cfg, strings.Join(urls, "\n"))
	s.manager.execute = func(_ context.Context, _ *pgxpool.Pool, _ Config, _ bool) int { return exitCanceled }
	runID, err := s.manager.startScheduled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitScheduled(t, s.manager)
	_, err = pool.Exec(ctx, `INSERT INTO audit_change_events(run_id,target_id,safe_url,kind,severity)
	 SELECT run_id,target_id,'https://example.com/'||target_id,'http_error','critical' FROM audit_run_targets WHERE run_id=$1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	const token = "notification-test-token-not-a-production-secret"
	s.web = appconfig.WebConfig{AccessToken: token, MaxURLs: 10000}
	s.queries = make(chan struct{}, 3)
	handler := s.handler()
	type page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		Next string `json:"next"`
	}
	get := func(cursor string) page {
		t.Helper()
		req := httptest.NewRequest("GET", "http://127.0.0.1/api/changes?run_id="+runID+"&before="+cursor, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("API: %d %s", w.Code, w.Body.String())
		}
		var result page
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := get("")
	second := get(first.Next)
	if len(first.Items) != 100 || first.Next == "" || len(second.Items) != 5 || second.Next != "" {
		t.Fatalf("pages: %d/%d next=%q/%q", len(first.Items), len(second.Items), first.Next, second.Next)
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, item := range first.Items {
		seen[item.ID] = true
		ids = append(ids, item.ID)
	}
	for _, item := range second.Items {
		if seen[item.ID] {
			t.Fatal("pagination duplicated an event")
		}
	}
	body, _ := json.Marshal(map[string]any{"ids": ids})
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/changes/read", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SEO-Auditor-Request", "1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("mark read: %d %s", w.Code, w.Body.String())
	}
	var unread int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1 AND read_at IS NULL`, runID).Scan(&unread); err != nil || unread != 5 {
		t.Fatalf("unread=%d err=%v", unread, err)
	}
}
