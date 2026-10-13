//go:build integration

package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func TestCrawlExtensionsUpgrade34PreservesSEOAndMonitoring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
	schema := "upgrade37_" + strings.ReplaceAll(newWebRunID(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`)
	}()
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
	if err = goose.UpToContext(ctx, db, migrationDir, 34); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := Config{RunID: newWebRunID(), WorkerInstanceID: "upgrade37", TargetFingerprintKey: []byte("local-development-only-fingerprint-key"), TargetFingerprintKeyID: "default", DBWriteTimeout: 3 * time.Second, DBFetchTimeout: 5 * time.Second, OwnerGeneration: 1}
	if err = createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,title) VALUES($1,1,'https://example.com/',$2,'default',200,'Existing SEO report')`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "https://example.com/")); err != nil {
		t.Fatal(err)
	}
	geoID, botID := newWebRunID(), newWebRunID()
	if _, err = pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count) VALUES($1,$2,$3,'example.com','local-lexical-v4',1,1,1,0)`, geoID, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, "fixture")); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO audit_bot_imports(id,domain,digest,fingerprint_key_id,format,stats,range_evidence) VALUES($1,'example.com',$2,'default','jsonl','{}','{}')`, botID, fingerprintURL(cfg.TargetFingerprintKey, "bot-fixture")); err != nil {
		t.Fatal(err)
	}
	input := scheduleInput{ID: newWebRunID(), Name: "Existing adaptive profile", Mode: "list", URLs: "https://example.com/", IntervalHours: 24, NextRunAt: time.Now().Add(time.Hour), Adaptive: true, MaxIntervalHours: 48, FailureThreshold: 2}
	source, label, count, err := prepareSchedule(&input, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	app := extensionApp(cfg)
	app.pool = pool
	if err = app.storeSchedule(ctx, input, source, label, count); err != nil {
		t.Fatal(err)
	}
	if err = applySchemaMigrationsDB(ctx, db); err != nil {
		t.Fatal(err)
	}
	var title string
	var preserved int
	if err = pool.QueryRow(ctx, `SELECT title FROM audit_results WHERE run_id=$1`, cfg.RunID).Scan(&title); err != nil || title != "Existing SEO report" {
		t.Fatalf("SEO changed: %q %v", title, err)
	}
	for _, query := range []string{`SELECT COUNT(*) FROM geo_reports WHERE id=$1`, `SELECT COUNT(*) FROM audit_bot_imports WHERE id=$1`, `SELECT COUNT(*) FROM audit_adaptive_targets WHERE schedule_id=$1`} {
		id := geoID
		if strings.Contains(query, "bot_imports") {
			id = botID
		}
		if strings.Contains(query, "adaptive_targets") {
			id = input.ID
		}
		if err = pool.QueryRow(ctx, query, id).Scan(&preserved); err != nil || preserved != 1 {
			t.Fatalf("existing data lost: %s %d %v", query, preserved, err)
		}
	}
	if err = applySchemaMigrationsDB(ctx, db); err != nil {
		t.Fatal("migration is not idempotent:", err)
	}
}
