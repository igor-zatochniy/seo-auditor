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

func TestGEOCorpusBrowserWorkflow(t *testing.T) {
	if os.Getenv("QUALITY_BROWSER") != "1" {
		t.Skip("Для браузерної перевірки потрібні QUALITY_BROWSER=1 та Playwright")
	}
	app, cfg := largeCorpusFixture(t)
	ctx := t.Context()
	if _, err := app.pool.Exec(ctx, `UPDATE audit_results SET title=title||' <img src=x onerror="window.geoCorpusXss=1">' WHERE run_id=$1 AND target_id=10001`, cfg.RunID); err != nil {
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
	check, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(check, node, "web/e2e/geo-corpus.cjs", server.URL, fixture.URL, testWebToken, cfg.RunID)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Перевірка браузера: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
