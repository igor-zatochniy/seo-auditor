//go:build integration

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
)

func TestScheduledURLChangesUseFingerprints(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	for _, tc := range []struct {
		name     string
		mutate   func(*crawler.URLSignals, *crawler.URLSignals)
		legacy   bool
		expected int
	}{
		{name: "changed_sensitive_query", expected: 2},
		{name: "unchanged_identity", mutate: func(old, current *crawler.URLSignals) { current.HTML.Fingerprints = old.HTML.Fingerprints }},
		{name: "equivalent_root_variants", mutate: func(old, current *crawler.URLSignals) {
			old.HTML.Fingerprints = []string{"root", "slash"}
			current.HTML.Fingerprints = []string{"slash", "root"}
		}},
		{name: "incomplete_current", mutate: func(_, current *crawler.URLSignals) { current.Complete = false }},
		{name: "incomplete_baseline", mutate: func(old, _ *crawler.URLSignals) { old.Complete = false }},
		{name: "truncated_reference", mutate: func(_, current *crawler.URLSignals) { current.HTML.Truncated = true }},
		{name: "invalid_reference", mutate: func(_, current *crawler.URLSignals) { current.HTML.Valid = false }},
		{name: "missing_identity", mutate: func(old, current *crawler.URLSignals) { old.HTML.Fingerprints, current.HTML.Fingerprints = nil, nil }},
		{name: "unknown_signal_version", mutate: func(_, current *crawler.URLSignals) { current.Version = "future-version" }},
		{name: "legacy_equal_redacted_urls", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackSiteTransaction(tx, cfg)
			_, err = tx.Exec(ctx, `CREATE TEMP TABLE audit_results (
 run_id UUID,target_id BIGINT,safe_url TEXT,target_fingerprint BYTEA,fingerprint_key_id TEXT,
 status_code INTEGER,scan_status TEXT,geo_signals JSONB,meta_robots TEXT DEFAULT '',x_robots_tag TEXT DEFAULT '',
 robots_outcome TEXT DEFAULT 'allowed',error_code TEXT DEFAULT '',canonical_url_truncated BOOLEAN DEFAULT FALSE,
 canonical_url TEXT DEFAULT '',is_redirect BOOLEAN DEFAULT FALSE,redirect_url TEXT DEFAULT '',redirect_url_truncated BOOLEAN DEFAULT FALSE,
 title TEXT DEFAULT 'title',description TEXT DEFAULT 'description',h1_count INTEGER DEFAULT 1,h1 TEXT DEFAULT 'h1',
 html_size_complete BOOLEAN DEFAULT TRUE,word_count INTEGER DEFAULT 100,url_signals JSONB
 ) ON COMMIT DROP;
 CREATE TEMP TABLE audit_change_events (
 run_id UUID,target_id BIGINT,safe_url TEXT,kind TEXT,severity TEXT,before_value TEXT,after_value TEXT,
 UNIQUE(run_id,target_id,kind)
 ) ON COMMIT DROP`)
			if err != nil {
				t.Fatal(err)
			}
			oldRef := secureURLReference(crawler.URLReference{URL: "https://example.com/destination?token=first-secret"}, cfg, true)
			newRef := secureURLReference(crawler.URLReference{URL: "https://example.com/destination?token=second-secret"}, cfg, true)
			old := crawler.URLSignals{Version: crawler.URLSignalsVersion, Complete: true, HTML: &oldRef}
			current := crawler.URLSignals{Version: crawler.URLSignalsVersion, Complete: true, HTML: &newRef}
			if tc.mutate != nil {
				tc.mutate(&old, &current)
			}
			previousRun, currentRun := newWebRunID(), newWebRunID()
			for i, signals := range []*crawler.URLSignals{&old, &current} {
				runID := previousRun
				if i == 1 {
					runID = currentRun
				}
				for targetID := int64(1); targetID <= 2; targetID++ {
					value := *signals
					status, scanStatus, canonical, redirect := 200, "completed", signals.HTML.URL, ""
					if targetID == 2 {
						value.Redirect, value.HTML = signals.HTML, nil
						status, scanStatus, canonical, redirect = 301, "redirect", "", signals.HTML.URL
					}
					var encoded any
					if !tc.legacy {
						data, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						encoded = data
					}
					_, err = tx.Exec(ctx, `INSERT INTO audit_results
 (run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,canonical_url,is_redirect,redirect_url,url_signals)
 VALUES($1,$2,'https://example.com/source',$3,'test-key',$4,$5,$6,$7,$8,$9)`,
						runID, targetID, []byte{byte(targetID)}, status, scanStatus, canonical, targetID == 2, redirect, encoded)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 {
				if _, err := tx.Exec(ctx, scheduledChangesSQL, currentRun, previousRun); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			var exported string
			if err := tx.QueryRow(ctx, `SELECT COUNT(*),COALESCE(jsonb_agg(to_jsonb(e))::TEXT,'') FROM audit_change_events e`).Scan(&count, &exported); err != nil {
				t.Fatal(err)
			}
			if count != tc.expected {
				t.Fatalf("expected %d idempotent URL change events, got %d: %s", tc.expected, count, exported)
			}
			if strings.Contains(exported, "first-secret") || strings.Contains(exported, "second-secret") {
				t.Fatal("query secret leaked into change event")
			}
		})
	}
}
