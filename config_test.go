package main

import (
	"log/slog"
	"testing"
)

var configEnvironmentVariables = []string{
	"DATABASE_URL",
	"RUN_ID",
	"TARGET_FINGERPRINT_KEY",
	"LOG_LEVEL",
	"WORKERS",
	"HTTP_ATTEMPT_TIMEOUT",
	"HTTP_TOTAL_TIMEOUT",
	"ROBOTS_ATTEMPT_TIMEOUT",
	"ROBOTS_TOTAL_TIMEOUT",
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

const testTargetFingerprintKey = "local-development-only-fingerprint-key"

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
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("WORKERS", "many")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid WORKERS to fail configuration loading")
	}
}

func TestLoadConfigUsesSafeDefaultsAndConfiguredLogLevel(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("LOG_LEVEL", "WARN")
	const runID = "9d532d38-2142-4f5a-9b68-6351ef5ed18c"
	t.Setenv("RUN_ID", runID)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("unexpected log level: %s", cfg.LogLevel)
	}
	if cfg.RunID != runID {
		t.Fatalf("unexpected run ID: %q", cfg.RunID)
	}
	if string(cfg.TargetFingerprintKey) != testTargetFingerprintKey {
		t.Fatalf("unexpected target fingerprint key")
	}
	if cfg.HTTPMaxRetries != DefaultHTTPMaxRetries || cfg.DBMaxRetries != DefaultDBMaxRetries {
		t.Fatalf("unexpected retry defaults: HTTP=%d DB=%d", cfg.HTTPMaxRetries, cfg.DBMaxRetries)
	}
	if cfg.HTTPAttemptTimeout != DefaultHTTPAttemptTimeout || cfg.HTTPTotalTimeout != DefaultHTTPTotalTimeout {
		t.Fatalf("unexpected HTTP timeout defaults: attempt=%s total=%s", cfg.HTTPAttemptTimeout, cfg.HTTPTotalTimeout)
	}
	if cfg.RobotsAttemptTimeout != DefaultRobotsAttemptTimeout || cfg.RobotsTotalTimeout != DefaultRobotsTotalTimeout {
		t.Fatalf("unexpected robots timeout defaults: attempt=%s total=%s", cfg.RobotsAttemptTimeout, cfg.RobotsTotalTimeout)
	}
}

func TestLoadConfigRejectsNonUUIDRunID(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("RUN_ID", "ci-run-42")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected non-UUID RUN_ID to fail configuration loading")
	}
}

func TestLoadConfigGeneratesUUIDRunID(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if !runIDPattern.MatchString(cfg.RunID) {
		t.Fatalf("generated run ID is not a canonical UUID: %q", cfg.RunID)
	}
}

func TestLoadConfigRejectsInvertedRetryDelays(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("RETRY_BASE_DELAY", "3s")
	t.Setenv("RETRY_MAX_DELAY", "1s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected inverted retry delays to fail configuration loading")
	}
}

func TestLoadConfigRejectsShortTargetFingerprintKey(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", "short")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected short TARGET_FINGERPRINT_KEY to fail configuration loading")
	}
}

func TestLoadConfigRejectsInvertedHTTPTimeouts(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("HTTP_ATTEMPT_TIMEOUT", "15s")
	t.Setenv("HTTP_TOTAL_TIMEOUT", "10s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected inverted HTTP timeouts to fail configuration loading")
	}
}

func TestLoadConfigRejectsInvertedRobotsTimeouts(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("ROBOTS_ATTEMPT_TIMEOUT", "15s")
	t.Setenv("ROBOTS_TOTAL_TIMEOUT", "10s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected inverted robots timeouts to fail configuration loading")
	}
}

func TestLoadConfigRejectsRetryDelayAboveHTTPBudget(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://user:test-placeholder-not-a-secret@postgres:5432/seo_db")
	t.Setenv("TARGET_FINGERPRINT_KEY", testTargetFingerprintKey)
	t.Setenv("RETRY_MAX_DELAY", "25s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected retry max delay above HTTP budget to fail configuration loading")
	}
}
