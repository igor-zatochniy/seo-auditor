package main

import (
	"log/slog"
	"testing"
)

var configEnvironmentVariables = []string{
	"DATABASE_URL",
	"RUN_ID",
	"LOG_LEVEL",
	"WORKERS",
	"HTTP_REQUEST_TIMEOUT",
	"ROBOTS_TIMEOUT",
	"DB_CONNECT_TIMEOUT",
	"DB_FETCH_TIMEOUT",
	"DB_WRITE_TIMEOUT",
	"SHUTDOWN_TIMEOUT",
	"URL_BATCH_SIZE",
	"MAX_HTML_BODY_BYTES",
	"RATE_LIMIT_INTERVAL",
	"MAX_CONCURRENT_PER_HOST",
	"ROBOTS_CACHE_TTL",
	"ALLOW_PRIVATE_TARGETS",
	"HTTP_MAX_RETRIES",
	"DB_MAX_RETRIES",
	"RETRY_BASE_DELAY",
	"RETRY_MAX_DELAY",
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range configEnvironmentVariables {
		t.Setenv(name, "")
	}
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	clearConfigEnvironment(t)

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected missing DATABASE_URL to fail configuration loading")
	}
}

func TestLoadConfigRejectsInvalidExplicitValue(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:password@postgres:5432/seo_db")
	t.Setenv("WORKERS", "many")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid WORKERS to fail configuration loading")
	}
}

func TestLoadConfigUsesSafeDefaultsAndConfiguredLogLevel(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:password@postgres:5432/seo_db")
	t.Setenv("LOG_LEVEL", "WARN")
	t.Setenv("RUN_ID", "ci-run-42")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("unexpected log level: %s", cfg.LogLevel)
	}
	if cfg.RunID != "ci-run-42" {
		t.Fatalf("unexpected run ID: %q", cfg.RunID)
	}
	if cfg.HTTPMaxRetries != DefaultHTTPMaxRetries || cfg.DBMaxRetries != DefaultDBMaxRetries {
		t.Fatalf("unexpected retry defaults: HTTP=%d DB=%d", cfg.HTTPMaxRetries, cfg.DBMaxRetries)
	}
}

func TestLoadConfigRejectsInvertedRetryDelays(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:password@postgres:5432/seo_db")
	t.Setenv("RETRY_BASE_DELAY", "3s")
	t.Setenv("RETRY_MAX_DELAY", "1s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected inverted retry delays to fail configuration loading")
	}
}
