package main

import (
	"testing"

	"github.com/pressly/goose/v3"
)

func TestEmbeddedMigrationsAreCollectable(t *testing.T) {
	goose.SetBaseFS(migrationFiles)
	t.Cleanup(func() {
		goose.SetBaseFS(nil)
	})

	migrations, err := goose.CollectMigrations(migrationDir, 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect embedded migrations: %v", err)
	}
	if len(migrations) != int(requiredSchemaVersion) {
		t.Fatalf("unexpected migration count: got %d want %d", len(migrations), requiredSchemaVersion)
	}
	if got := migrations[len(migrations)-1].Version; got != requiredSchemaVersion {
		t.Fatalf("unexpected latest migration version: got %d want %d", got, requiredSchemaVersion)
	}
}
