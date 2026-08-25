package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const (
	migrationDir            = "initdb"
	migrationVersionTable   = "schema_migrations"
	requiredSchemaVersion   = int64(11)
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

	if err := sanitizeLegacyURLColumnsBeforeMigration004(ctx, db); err != nil {
		return err
	}

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

type legacyMigrationURLColumn struct {
	table    string
	column   string
	identity bool
}

func sanitizeLegacyURLColumnsBeforeMigration004(ctx context.Context, db *sql.DB) error {
	version, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version before legacy URL sanitization: %w", err)
	}
	if version >= 4 {
		return nil
	}

	columns := []legacyMigrationURLColumn{
		{table: "seo_results", column: "url", identity: true},
		{table: "seo_results", column: "redirect_url"},
		{table: "seo_results", column: "canonical_url"},
		{table: "seo_results", column: "og_image"},
		{table: "audit_results", column: "url", identity: true},
		{table: "audit_results", column: "safe_url", identity: true},
		{table: "audit_results", column: "redirect_url"},
		{table: "audit_results", column: "canonical_url"},
		{table: "audit_results", column: "og_image"},
	}
	for _, candidate := range columns {
		exists, err := migrationColumnExists(ctx, db, candidate)
		if err != nil {
			return fmt.Errorf("inspect legacy URL column %s.%s: %w", candidate.table, candidate.column, err)
		}
		if !exists {
			continue
		}

		table := quoteMigrationIdentifier(candidate.table)
		column := quoteMigrationIdentifier(candidate.column)
		statement := ""
		if candidate.identity {
			statement = fmt.Sprintf(
				`UPDATE %s
				 SET %s = 'https://redacted.invalid/legacy/%s/' || id::TEXT
				 WHERE %s LIKE '%%?%%' OR %s LIKE '%%#%%'`,
				table,
				column,
				candidate.table,
				column,
				column,
			)
		} else {
			statement = fmt.Sprintf(
				`UPDATE %s
			 SET %s = LEFT(split_part(split_part(%s, '#', 1), '?', 1), 2048)
			 WHERE %s LIKE '%%?%%' OR %s LIKE '%%#%%'`,
				table,
				column,
				column,
				column,
				column,
			)
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sanitize legacy URL column %s.%s: %w", candidate.table, candidate.column, err)
		}
	}
	return nil
}

func currentMigrationVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var exists bool
	if err := db.QueryRowContext(
		ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = current_schema()
			  AND table_name = $1
		)`,
		migrationVersionTable,
	).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	return goose.GetDBVersionContext(ctx, db)
}

func migrationColumnExists(
	ctx context.Context,
	db *sql.DB,
	candidate legacyMigrationURLColumn,
) (bool, error) {
	var exists bool
	err := db.QueryRowContext(
		ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND column_name = $2
		)`,
		candidate.table,
		candidate.column,
	).Scan(&exists)
	return exists, err
}

func quoteMigrationIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
