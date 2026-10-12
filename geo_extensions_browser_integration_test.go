//go:build integration

package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGEOExtensionsBrowserWorkflow(t *testing.T) {
	if os.Getenv("QUALITY_BROWSER") != "1" {
		t.Skip("Потрібні QUALITY_BROWSER=1 та Playwright")
	}
	app, _, cfg := visibilityReportFixture(t, 3*time.Minute)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, "DELETE FROM geo_corpora WHERE run_id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(ctx, "UPDATE audit_results SET title='Stripe payment protection',h1='Stripe payment protection' WHERE run_id=$1 AND target_id=1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	app.manager = newAuditManager(ctx, app.pool, cfg)
	app.manager.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	defer app.manager.shutdown()
	server := httptest.NewServer(app.handler())
	defer server.Close()
	fixture := webFixture()
	defer fixture.Close()
	node := os.Getenv("QUALITY_NODE_EXECUTABLE")
	if node == "" {
		node = "node"
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(check, node, "web/e2e/geo-extensions.cjs", server.URL, fixture.URL, testWebToken, cfg.RunID)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Браузер: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
