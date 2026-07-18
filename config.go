package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHTTPMaxRetries = 2
	DefaultDBMaxRetries   = 2
	MaxRetryAttempts      = 5
	DefaultRetryBaseDelay = 200 * time.Millisecond
	DefaultRetryMaxDelay  = 2 * time.Second
)

var runIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Config struct {
	RunID                string
	LogLevel             slog.Level
	Workers              int
	DatabaseURL          string
	HTTPRequestTimeout   time.Duration
	RobotsTimeout        time.Duration
	DBConnectTimeout     time.Duration
	DBFetchTimeout       time.Duration
	DBWriteTimeout       time.Duration
	ShutdownTimeout      time.Duration
	URLBatchSize         int
	MaxHTMLBodyBytes     int64
	AllowPrivateTargets  bool
	RateLimitInterval    time.Duration
	MaxConcurrentPerHost int
	RobotsCacheTTL       time.Duration
	HTTPMaxRetries       int
	DBMaxRetries         int
	RetryBaseDelay       time.Duration
	RetryMaxDelay        time.Duration
}

func loadConfig() (Config, error) {
	workers, err := intFromEnv("WORKERS", DefaultWorkers, 1, MaxWorkers)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := durationFromEnv("HTTP_REQUEST_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	robotsTimeout, err := durationFromEnv("ROBOTS_TIMEOUT", 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbConnectTimeout, err := durationFromEnv("DB_CONNECT_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbFetchTimeout, err := durationFromEnv("DB_FETCH_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbWriteTimeout, err := durationFromEnv("DB_WRITE_TIMEOUT", 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationFromEnv("SHUTDOWN_TIMEOUT", DefaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}
	urlBatchSize, err := intFromEnv("URL_BATCH_SIZE", DefaultURLBatchSize, 1, MaxURLBatchSize)
	if err != nil {
		return Config{}, err
	}
	maxHTMLBodyBytes, err := int64FromEnv("MAX_HTML_BODY_BYTES", DefaultMaxHTMLBodyBytes, 1024, 50*1024*1024)
	if err != nil {
		return Config{}, err
	}
	allowPrivateTargets, err := boolFromEnv("ALLOW_PRIVATE_TARGETS", false)
	if err != nil {
		return Config{}, err
	}
	rateLimitInterval, err := durationFromEnv("RATE_LIMIT_INTERVAL", 500*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	maxConcurrentPerHost, err := intFromEnv("MAX_CONCURRENT_PER_HOST", DefaultMaxConcurrentPerHost, 1, MaxPerHostConcurrency)
	if err != nil {
		return Config{}, err
	}
	robotsCacheTTL, err := durationFromEnv("ROBOTS_CACHE_TTL", DefaultRobotsCacheTTL)
	if err != nil {
		return Config{}, err
	}
	if robotsCacheTTL > MaxRobotsCacheTTL {
		return Config{}, fmt.Errorf("ROBOTS_CACHE_TTL must not exceed %s", MaxRobotsCacheTTL)
	}
	httpMaxRetries, err := intFromEnv("HTTP_MAX_RETRIES", DefaultHTTPMaxRetries, 0, MaxRetryAttempts)
	if err != nil {
		return Config{}, err
	}
	dbMaxRetries, err := intFromEnv("DB_MAX_RETRIES", DefaultDBMaxRetries, 0, MaxRetryAttempts)
	if err != nil {
		return Config{}, err
	}
	retryBaseDelay, err := durationFromEnv("RETRY_BASE_DELAY", DefaultRetryBaseDelay)
	if err != nil {
		return Config{}, err
	}
	retryMaxDelay, err := durationFromEnv("RETRY_MAX_DELAY", DefaultRetryMaxDelay)
	if err != nil {
		return Config{}, err
	}
	if retryBaseDelay > retryMaxDelay {
		return Config{}, fmt.Errorf("RETRY_BASE_DELAY must not exceed RETRY_MAX_DELAY")
	}
	logLevel, err := logLevelFromEnv("LOG_LEVEL", slog.LevelInfo)
	if err != nil {
		return Config{}, err
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	runID, err := runIDFromEnv()
	if err != nil {
		return Config{}, err
	}

	return Config{
		RunID:                runID,
		LogLevel:             logLevel,
		Workers:              workers,
		DatabaseURL:          databaseURL,
		HTTPRequestTimeout:   requestTimeout,
		RobotsTimeout:        robotsTimeout,
		DBConnectTimeout:     dbConnectTimeout,
		DBFetchTimeout:       dbFetchTimeout,
		DBWriteTimeout:       dbWriteTimeout,
		ShutdownTimeout:      shutdownTimeout,
		URLBatchSize:         urlBatchSize,
		MaxHTMLBodyBytes:     maxHTMLBodyBytes,
		AllowPrivateTargets:  allowPrivateTargets,
		RateLimitInterval:    rateLimitInterval,
		MaxConcurrentPerHost: maxConcurrentPerHost,
		RobotsCacheTTL:       robotsCacheTTL,
		HTTPMaxRetries:       httpMaxRetries,
		DBMaxRetries:         dbMaxRetries,
		RetryBaseDelay:       retryBaseDelay,
		RetryMaxDelay:        retryMaxDelay,
	}, nil
}

func intFromEnv(name string, fallback, minValue, maxValue int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minValue, maxValue)
	}
	return value, nil
}

func int64FromEnv(name string, fallback, minValue, maxValue int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minValue, maxValue)
	}
	return value, nil
}

func durationFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return value, nil
}

func boolFromEnv(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

func logLevelFromEnv(name string, fallback slog.Level) (slog.Level, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be one of DEBUG, INFO, WARN or ERROR", name)
	}
	return level, nil
}

func runIDFromEnv() (string, error) {
	if raw := strings.TrimSpace(os.Getenv("RUN_ID")); raw != "" {
		if !runIDPattern.MatchString(raw) {
			return "", fmt.Errorf("RUN_ID must be a canonical UUID")
		}
		return strings.ToLower(raw), nil
	}

	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	randomBytes[6] = (randomBytes[6] & 0x0f) | 0x40
	randomBytes[8] = (randomBytes[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		randomBytes[0:4],
		randomBytes[4:6],
		randomBytes[6:8],
		randomBytes[8:10],
		randomBytes[10:16],
	), nil
}
