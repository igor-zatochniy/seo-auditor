//go:build integration

package main

import (
	"context"
	"database/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsUpgrade37PreservesHistoricalData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn, e := url.Parse(os.Getenv("DATABASE_URL"))
	if e != nil || dsn.Host == "" {
		t.Fatal("DATABASE_URL required")
	}
	admin, e := pgxpool.New(ctx, dsn.String())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "diagnostics39_" + strings.ReplaceAll(newWebRunID(), "-", "")
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`)
	}()
	q := dsn.Query()
	q.Set("search_path", schema)
	dsn.RawQuery = q.Encode()
	db, e := sql.Open("pgx", dsn.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	goose.SetBaseFS(migrationFiles)
	goose.SetTableName(migrationVersionTable)
	goose.SetLogger(goose.NopLogger())
	if e = goose.SetDialect("pgx"); e != nil {
		t.Fatal(e)
	}
	if e = goose.UpToContext(ctx, db, migrationDir, 37); e != nil {
		t.Fatal(e)
	}
	pool, e := pgxpool.New(ctx, dsn.String())
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	cfg := Config{RunID: newWebRunID(), WorkerInstanceID: "diagnostics-upgrade", TargetFingerprintKey: []byte("local-development-only-upgrade-key"), TargetFingerprintKeyID: "default", DBWriteTimeout: 3 * time.Second, DBFetchTimeout: 5 * time.Second, OwnerGeneration: 1}
	if e = createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/"}); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,title,url_signals)
 VALUES($1,1,'https://example.com/',$2,'default',200,'Historical SEO', '{"version":"url-signals-v1","complete":true}')`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/")); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO audit_site_crawls(run_id,origin,root_safe_url,key_check,max_pages,max_depth,use_sitemaps,sitemap_state) VALUES($1,'https://example.com','https://example.com/',$2,100,2,FALSE,'disabled')`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "site-crawl:"+cfg.RunID)); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO audit_site_nodes(run_id,target_id,url_fingerprint,safe_url,frontier_depth) VALUES($1,1,$2,'https://example.com/',0)`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/")); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO audit_crux_observations(run_id,target_id,device,scope,report) VALUES($1,1,'PHONE','url','{"status":"no_data"}')`, cfg.RunID); e != nil {
		t.Fatal(e)
	}
	var before string
	if e = pool.QueryRow(ctx, `SELECT row_to_json(r)::TEXT FROM audit_results r WHERE run_id=$1`, cfg.RunID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	if e = applySchemaMigrationsDB(ctx, db); e != nil {
		t.Fatal(e)
	}
	if e = applySchemaMigrationsDB(ctx, db); e != nil {
		t.Fatal("idempotency", e)
	}
	var after string
	_ = pool.QueryRow(ctx, `SELECT row_to_json(r)::TEXT FROM audit_results r WHERE run_id=$1`, cfg.RunID).Scan(&after)
	if before != after {
		t.Fatal("historical SEO data rewritten")
	}
	app := extensionApp(cfg)
	app.pool = pool
	w := extensionAPI(t, app, "GET", "/api/audits/"+cfg.RunID+"/coverage", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"legacy":true`) && !strings.Contains(w.Body.String(), `"legacy": true`) {
		t.Fatal("legacy coverage hidden", w.Code, w.Body.String())
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_crux_observations WHERE run_id=$1`, cfg.RunID).Scan(&count)
	if count != 1 {
		t.Fatal("CrUX observation lost")
	}
	if e = goose.DownToContext(ctx, db, migrationDir, 37); e != nil {
		t.Fatal("rollback", e)
	}
	_ = pool.QueryRow(ctx, `SELECT row_to_json(r)::TEXT FROM audit_results r WHERE run_id=$1`, cfg.RunID).Scan(&after)
	if before != after {
		t.Fatal("rollback damaged old SEO")
	}
}
