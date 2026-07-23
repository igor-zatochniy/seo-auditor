package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const (
	migrationDir            = "initdb"
	migrationVersionTable   = "schema_migrations"
	requiredSchemaVersion   = int64(6)
	postgresMigrationDriver = "pgx"
	migrationAdvisoryLockID = int64(8_247_310_003)
	migrationUnlockTimeout  = 5 * time.Second
)

//go:embed initdb/*.sql
var migrationFiles embed.FS

func applySchemaMigrations(ctx context.Context, cfg Config) error {
	db, err := sql.Open(postgresMigrationDriver, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping migration connection: %w", err)
	}

	return applySchemaMigrationsDB(ctx, db)
}

func applySchemaMigrationsDB(ctx context.Context, db *sql.DB) error {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	goose.SetBaseFS(migrationFiles)
	goose.SetTableName(migrationVersionTable)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(postgresMigrationDriver); err != nil {
		return fmt.Errorf("configure migration dialect: %w", err)
	}

	if _, err := db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), migrationUnlockTimeout)
		defer cancel()
		if _, err := db.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockID); err != nil {
			slog.Error("Не вдалося звільнити lock міграцій PostgreSQL", "error", err)
		}
	}()

	if err := goose.UpContext(ctx, db, migrationDir); err != nil {
		return fmt.Errorf("apply schema migrations: %w", err)
	}

	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version < requiredSchemaVersion {
		return fmt.Errorf("schema version %d is lower than required %d", version, requiredSchemaVersion)
	}
	if version > requiredSchemaVersion {
		return fmt.Errorf("schema version %d is newer than supported %d", version, requiredSchemaVersion)
	}

	slog.Info("Схему PostgreSQL перевірено", "schema_version", version)
	return nil
}
