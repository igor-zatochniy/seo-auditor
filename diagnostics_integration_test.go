//go:build integration

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestGEODiagnosticsSurviveHTTPFailuresWithoutRendering(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /blocked\nUser-agent: OAI-SearchBot\nAllow: /\nUser-agent: GPTBot\nDisallow: /\n")
			return
		}
		if r.URL.Path == "/error" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<main><h1>Захист платежів</h1><p>"+strings.Repeat("я", 350)+"</p></main>")
	}))
	defer fixture.Close()
	if err := createExplicitAuditRun(ctx, pool, &cfg, []string{fixture.URL + "/page", fixture.URL + "/error", fixture.URL + "/blocked"}); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatalf("unexpected exit %d", code)
	}
	rows, err := pool.Query(ctx, "SELECT scan_status,status_code,geo_signals,rendering FROM audit_results WHERE run_id=$1 ORDER BY target_id", cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		i++
		var status string
		var code *int
		var signals *geo.Signals
		var rendering []byte
		if err := rows.Scan(&status, &code, &signals, &rendering); err != nil {
			t.Fatal(err)
		}
		if signals == nil || signals.Search.OAISearchBotRules != geo.Allowed || signals.Search.GPTBotRules != geo.Blocked || len(rendering) > 0 {
			t.Fatalf("lost rules or unexpected browser at %d: %+v", i, signals)
		}
		if i == 1 && (signals.Blocks == nil || signals.Blocks.Count != 1 || signals.HTTP == nil || signals.HTTP.ResponseMS == nil || signals.Performance != nil) {
			t.Fatalf("bad HTML diagnostics: %+v", signals)
		}
		if i == 2 && (code == nil || *code != 403 || signals.HTTP == nil || signals.HTTP.Status != 403) {
			t.Fatal("HTTP error observation lost")
		}
		if i == 3 && (code != nil || status != scanStatusBlockedByRobots || signals.HTTP != nil) {
			t.Fatal("invented HTTP request for robots-blocked URL")
		}
	}
	if rows.Err() != nil || i != 3 {
		t.Fatalf("incomplete rows: %d %v", i, rows.Err())
	}
}
