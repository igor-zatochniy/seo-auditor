package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/html/charset"
)

const (
	MinTitleLen       = 40
	MaxTitleLen       = 65
	MinDescriptionLen = 120
	MaxDescriptionLen = 170
	UserAgentStr      = "Go-SEOParser-Bot/1.0"

	DefaultWorkers          = 3
	DefaultURLBatchSize     = 100
	DefaultShutdownTimeout  = 25 * time.Second
	MaxURLBatchSize         = 10_000
	MaxWorkers              = 32
	QueueBufferPerWorker    = 2
	DefaultMaxHTMLBodyBytes = int64(5 * 1024 * 1024)
	MaxRobotsBodyBytes      = int64(512 * 1024)
	MaxRobotsRedirects      = 5
)

const (
	storageURLMaxRunes         = 2048
	storageTitleMaxRunes       = 500
	storageH1MaxRunes          = 1000
	storageOGTitleMaxRunes     = 500
	storageTwitterCardMaxRunes = 100
	storageRobotsTagMaxRunes   = 200
)

const (
	exitSuccess  = 0
	exitFatal    = 1
	exitCanceled = 130
)

const (
	scanStatusCompleted       = "completed"
	scanStatusRedirect        = "redirect"
	scanStatusBlockedByRobots = "blocked_by_robots"
	scanStatusFailed          = "failed"

	robotsOutcomeAllowed     = "allowed"
	robotsOutcomeDisallowed  = "disallowed"
	robotsOutcomeUnavailable = "unavailable"
	robotsOutcomeNotChecked  = "not_checked"

	errorCodeRobotsUnavailable      = "robots_unavailable"
	errorCodeRequestCreationFailed  = "request_creation_failed"
	errorCodeRequestFailed          = "request_failed"
	errorCodeMissingContentType     = "missing_content_type"
	errorCodeUnsupportedContentType = "unsupported_content_type"
	errorCodeResponseParseFailed    = "response_parse_failed"
	errorCodeInvalidTargetURL       = "invalid_target_url"
	errorCodeInternal               = "internal_error"
)

var blockedTargetIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// SEOData contains the full set of metrics collected by the parser.
type SEOData struct {
	URL                        string
	SafeURLTruncated           bool
	SafeURLOriginalLength      int
	StatusCode                 *int
	ScanStatus                 string
	ErrorCode                  string
	ErrorMessage               string
	IsRedirect                 bool
	RedirectURL                string
	RedirectURLTruncated       bool
	RedirectURLOriginalLength  int
	Title                      string
	TitleStatus                string
	TitleTruncated             bool
	TitleOriginalLength        int
	Description                string
	DescriptionStatus          string
	H1                         string
	H1Count                    int
	H1Truncated                bool
	H1OriginalLength           int
	H2ToH6Status               string
	OGTitle                    string
	OGTitleTruncated           bool
	OGTitleOriginalLength      int
	OGDescription              string
	OGImage                    string
	OGImageTruncated           bool
	OGImageOriginalLength      int
	TwitterCard                string
	TwitterCardTruncated       bool
	TwitterCardOriginalLength  int
	InternalLinksCount         int
	ExternalLinksCount         int
	LinksCount                 int
	CanonicalURL               string
	CanonicalURLTruncated      bool
	CanonicalURLOriginalLength int
	IsSelfCanonical            bool
	MetaRobots                 string
	MetaRobotsTruncated        bool
	MetaRobotsOriginalLength   int
	XRobotsTag                 string
	XRobotsTagTruncated        bool
	XRobotsTagOriginalLength   int
	RobotsAllowed              bool
	RobotsOutcome              string
	HasJsonLd                  bool
	HasViewport                bool
	TotalImages                int
	ImagesMissingAlt           int
	WordCount                  int
	Duration                   time.Duration
}

type Result struct {
	Target AuditTarget
	Data   SEOData
	Error  error
}

type ResultSummary struct {
	Received   int
	Saved      int
	Successful int
	Failed     int
}

func httpStatus(code int) *int {
	return &code
}

func failedScanResult(data SEOData, code string, err error) Result {
	data.ScanStatus = scanStatusFailed
	data.ErrorCode = code
	data.ErrorMessage = sanitizeError(err)
	return Result{Data: data, Error: errors.New(data.ErrorMessage)}
}

type targetURLRecord struct {
	ID  int64
	URL string
}

type AuditTarget struct {
	TargetID    int64
	RequestURL  string
	SafeURL     string
	Fingerprint []byte
}

type targetURLSnapshot struct {
	HighWatermark int64
	Total         int64
}

type urlStreamSummary struct {
	Queued  int
	Skipped int
	Error   error
}

type targetURLBatchFetcher func(ctx context.Context, afterID, highWatermark int64, limit int) ([]targetURLRecord, error)

type robotsRule struct {
	allow       bool
	pattern     string
	specificity int
}

type robotsGroup struct {
	agents []string
	rules  []robotsRule
}

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	bootstrapLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(bootstrapLogger)
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("Некоректна конфігурація рантайму", "error", err)
		return exitFatal
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).With("run_id", cfg.RunID)
	slog.SetDefault(logger)

	slog.Info("Запускається етичний SEO-аудитор")

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info(
		"Конфігурацію рантайму ініціалізовано",
		"worker_instance_id",
		cfg.WorkerInstanceID,
		"workers",
		cfg.Workers,
		"http_attempt_timeout",
		cfg.HTTPAttemptTimeout.String(),
		"http_total_timeout",
		cfg.HTTPTotalTimeout.String(),
		"robots_attempt_timeout",
		cfg.RobotsAttemptTimeout.String(),
		"robots_total_timeout",
		cfg.RobotsTotalTimeout.String(),
		"db_migration_timeout",
		cfg.DBMigrationTimeout.String(),
		"audit_run_heartbeat",
		cfg.AuditRunHeartbeatInterval.String(),
		"stale_run_threshold",
		cfg.StaleRunThreshold.String(),
		"shutdown_timeout",
		cfg.ShutdownTimeout.String(),
		"per_host_interval",
		cfg.RateLimitInterval.String(),
		"max_concurrent_per_host",
		cfg.MaxConcurrentPerHost,
		"robots_cache_ttl",
		cfg.RobotsCacheTTL.String(),
		"max_html_body_bytes",
		cfg.MaxHTMLBodyBytes,
	)

	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("Не вдалося розібрати рядок підключення до PostgreSQL", "error", err)
		return exitFatal
	}

	poolConfig.MaxConns = int32(cfg.Workers + 2)
	poolConfig.MinConns = 2
	if poolConfig.MinConns > poolConfig.MaxConns {
		poolConfig.MinConns = poolConfig.MaxConns
	}
	poolConfig.MaxConnIdleTime = 15 * time.Minute
	poolConfig.MaxConnLifetime = 1 * time.Hour

	dbInitCtx, dbCancel := context.WithTimeout(signalCtx, cfg.DBConnectTimeout)
	dbPool, err := pgxpool.NewWithConfig(dbInitCtx, poolConfig)
	if err != nil {
		dbCancel()
		slog.Error("Не вдалося ініціалізувати пул підключень PostgreSQL", "error", err)
		return exitFatal
	}

	dbRetryPolicy := retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay}
	if err := retryDBOperation(dbInitCtx, "ping", dbRetryPolicy, func() error {
		return dbPool.Ping(dbInitCtx)
	}); err != nil {
		dbCancel()
		dbPool.Close()
		slog.Error("PostgreSQL недоступний під час перевірки підключення", "error", err)
		return exitFatal
	}
	dbCancel()

	dbMigrationCtx, dbMigrationCancel := context.WithTimeout(signalCtx, cfg.DBMigrationTimeout)
	if err := applySchemaMigrations(dbMigrationCtx, cfg); err != nil {
		dbMigrationCancel()
		dbPool.Close()
		slog.Error("Не вдалося застосувати міграції PostgreSQL", "error", err)
		return exitFatal
	}
	dbMigrationCancel()

	dbMaintenanceCtx, dbMaintenanceCancel := context.WithTimeout(signalCtx, cfg.DBWriteTimeout)
	abandonedRuns, err := abandonStaleAuditRuns(dbMaintenanceCtx, dbPool, cfg)
	if err != nil {
		dbMaintenanceCancel()
		dbPool.Close()
		slog.Error("Не вдалося позначити застарілі запуски аудиту як abandoned", "error", err)
		return exitFatal
	}
	dbMaintenanceCancel()
	if abandonedRuns > 0 {
		slog.Warn("Застарілі running-запуски позначено як abandoned", "count", abandonedRuns)
	}

	dbRunCtx, dbRunCancel := context.WithTimeout(signalCtx, cfg.DBWriteTimeout)
	if err := createAuditRun(dbRunCtx, dbPool, cfg); err != nil {
		dbRunCancel()
		dbPool.Close()
		slog.Error("Не вдалося зареєструвати запуск аудиту", "error", err)
		return exitFatal
	}
	dbRunCancel()
	slog.Info(
		"Підключення до PostgreSQL підтверджено, схема актуальна та запуск аудиту зареєстровано",
		"max_conns",
		poolConfig.MaxConns,
	)

	defer func() {
		slog.Info("Закривається пул підключень PostgreSQL")
		dbPool.Close()
	}()
	runCompletion := auditRunCompletion{Status: auditRunStatusFailed}
	defer func() {
		completionCtx, completionCancel := context.WithTimeout(context.Background(), cfg.DBWriteTimeout)
		defer completionCancel()
		if err := completeAuditRun(completionCtx, dbPool, cfg.RunID, runCompletion, cfg); err != nil {
			slog.Error("Не вдалося завершити запис запуску аудиту", "error", err)
			exitCode = exitFatal
		}
	}()
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := startAuditRunHeartbeat(heartbeatCtx, dbPool, cfg)
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()

	targetSnapshot, err := captureAuditRunTargets(signalCtx, dbPool, cfg)
	if err != nil {
		if signalCtx.Err() != nil {
			runCompletion.Status = auditRunStatusCanceled
			slog.Warn("Запуск скасовано до фіксації стабільного набору цілей", "error", signalCtx.Err())
			return exitCanceled
		}
		slog.Error("Не вдалося зафіксувати стабільний набір цілей аудиту", "error", err)
		return exitFatal
	}
	if targetSnapshot.Total == 0 {
		slog.Warn("Стабільний набір цілей аудиту порожній")
		if signalCtx.Err() != nil {
			runCompletion.Status = auditRunStatusCanceled
			return exitCanceled
		}
		runCompletion.Status = auditRunStatusCompleted
		return exitSuccess
	}
	runCompletion.TotalURLs = targetSnapshot.Total
	slog.Info(
		"Зафіксовано стабільний набір цілей аудиту",
		"high_watermark",
		targetSnapshot.HighWatermark,
		"total_urls",
		targetSnapshot.Total,
		"batch_size",
		cfg.URLBatchSize,
	)

	queueCapacity := cfg.Workers * QueueBufferPerWorker
	jobs := make(chan AuditTarget, queueCapacity)
	results := make(chan Result, queueCapacity)

	pageCustomTransport := newHTTPTransport(cfg, cfg.HTTPAttemptTimeout)
	robotsCustomTransport := newHTTPTransport(cfg, cfg.RobotsAttemptTimeout)
	hostPolicies := newHostPolicyManager(
		cfg.RateLimitInterval,
		cfg.MaxConcurrentPerHost,
		DefaultHostStateCacheSize,
		MaxRetryAfterDelay,
	)
	pagePoliteTransport := &politeRoundTripper{
		base:     pageCustomTransport,
		policies: hostPolicies,
	}
	robotsPoliteTransport := &politeRoundTripper{
		base:     robotsCustomTransport,
		policies: hostPolicies,
	}
	pageRetryingTransport := &retryRoundTripper{
		base: pagePoliteTransport,
		policy: retryPolicy{
			maxRetries:     cfg.HTTPMaxRetries,
			attemptTimeout: cfg.HTTPAttemptTimeout,
			baseDelay:      cfg.RetryBaseDelay,
			maxDelay:       cfg.RetryMaxDelay,
		},
	}
	robotsRetryingTransport := &retryRoundTripper{
		base: robotsPoliteTransport,
		policy: retryPolicy{
			maxRetries:     cfg.HTTPMaxRetries,
			attemptTimeout: cfg.RobotsAttemptTimeout,
			baseDelay:      cfg.RetryBaseDelay,
			maxDelay:       cfg.RetryMaxDelay,
		},
	}
	robotsCache := newRobotsPolicyCache(cfg.RobotsCacheTTL, DefaultRobotsCacheMaxEntries)

	pageHTTPClient := &http.Client{
		Transport: pageRetryingTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	robotsHTTPClient := newRobotsHTTPClient(robotsRetryingTransport)
	defer pageCustomTransport.CloseIdleConnections()
	defer robotsCustomTransport.CloseIdleConnections()

	operationCtx, cancelOperations := context.WithCancel(context.WithoutCancel(signalCtx))
	defer cancelOperations()
	processingDone := make(chan struct{})
	shutdownGuardDone := guardGracefulShutdown(
		signalCtx,
		processingDone,
		cfg.ShutdownTimeout,
		cancelOperations,
	)

	var wg sync.WaitGroup
	for w := 1; w <= cfg.Workers; w++ {
		wg.Add(1)
		go worker(signalCtx, operationCtx, w, jobs, results, pageHTTPClient, robotsHTTPClient, robotsCache, dbPool, cfg, &wg)
	}

	streamDone := make(chan urlStreamSummary, 1)
	go func() {
		defer close(jobs)
		streamDone <- streamTargetURLs(
			signalCtx,
			targetSnapshot.HighWatermark,
			cfg.URLBatchSize,
			cfg,
			jobs,
			results,
			func(ctx context.Context, afterID, highWatermark int64, limit int) ([]targetURLRecord, error) {
				return fetchTargetURLBatch(ctx, dbPool, cfg, afterID, highWatermark, limit)
			},
		)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	slog.Info("Починається паралельна обробка URL та збереження результатів")
	summary := saveResults(operationCtx, dbPool, results, cfg)
	streamSummary := <-streamDone
	close(processingDone)
	<-shutdownGuardDone

	shutdownRequested := signalCtx.Err() != nil
	streamCanceledByShutdown := shutdownRequested && errors.Is(streamSummary.Error, context.Canceled)
	unprocessedTargets := targetSnapshot.Total - int64(streamSummary.Queued+streamSummary.Skipped)
	if unprocessedTargets < 0 {
		unprocessedTargets = 0
	}
	expectedResults := streamSummary.Queued + streamSummary.Skipped
	missingResults := expectedResults - summary.Received
	if missingResults < 0 {
		missingResults = 0
	}
	runCompletion.SuccessfulURLs = summary.Successful
	runCompletion.FailedURLs = summary.Failed + missingResults
	if streamSummary.Error != nil && !streamCanceledByShutdown {
		runCompletion.FailedURLs += int(unprocessedTargets)
		slog.Error(
			"Потокове читання стабільного набору цілей завершилося помилкою",
			"error",
			streamSummary.Error,
			"queued_urls",
			streamSummary.Queued,
			"saved_results",
			summary.Saved,
		)
		return exitFatal
	}

	if streamSummary.Queued == 0 {
		slog.Warn("Стабільний набір цілей не містить валідних URL", "skipped_urls", streamSummary.Skipped)
	}
	if shutdownRequested {
		runCompletion.Status = auditRunStatusCanceled
		slog.Warn(
			"Запуск аудиту скасовано до завершення стабільного набору цілей",
			"shutdown_requested",
			shutdownRequested,
			"unprocessed_targets",
			unprocessedTargets,
			"skipped_urls",
			streamSummary.Skipped,
			"failed_results",
			summary.Failed,
			"missing_results",
			missingResults,
			"saved_results",
			summary.Saved,
		)
		return exitCanceled
	}
	if unprocessedTargets > 0 {
		runCompletion.FailedURLs += int(unprocessedTargets)
		runCompletion.Status = auditRunStatusFailed
		slog.Error(
			"Стабільний набір цілей оброблено не повністю",
			"unprocessed_targets",
			unprocessedTargets,
			"skipped_urls",
			streamSummary.Skipped,
			"failed_results",
			summary.Failed,
			"missing_results",
			missingResults,
			"saved_results",
			summary.Saved,
		)
		return exitFatal
	}
	if streamSummary.Skipped > 0 || summary.Failed > 0 || missingResults > 0 {
		runCompletion.Status = auditRunStatusCompletedWithErrors
		slog.Warn(
			"Аудит завершено з помилками окремих URL",
			"skipped_urls",
			streamSummary.Skipped,
			"failed_results",
			summary.Failed,
			"missing_results",
			missingResults,
			"saved_results",
			summary.Saved,
		)
		return exitSuccess
	}

	runCompletion.Status = auditRunStatusCompleted
	slog.Info(
		"Роботу парсера завершено",
		"queued_urls",
		streamSummary.Queued,
		"saved_results",
		summary.Saved,
	)
	return exitSuccess
}

func guardGracefulShutdown(
	signalCtx context.Context,
	processingDone <-chan struct{},
	timeout time.Duration,
	cancelOperations context.CancelFunc,
) <-chan struct{} {
	guardDone := make(chan struct{})
	go func() {
		defer close(guardDone)

		select {
		case <-processingDone:
			return
		case <-signalCtx.Done():
			slog.Warn(
				"Отримано сигнал зупинки: нові URL більше не плануються",
				"shutdown_timeout",
				timeout.String(),
			)
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-processingDone:
			slog.Info("Поточні задачі та збереження результатів завершено в межах shutdown timeout")
		case <-timer.C:
			slog.Error("Вичерпано shutdown timeout, активні операції примусово скасовуються")
			cancelOperations()
		}
	}()

	return guardDone
}

func newHTTPTransport(cfg Config, attemptTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   attemptTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext:           safeDialContext(dialer, cfg.AllowPrivateTargets),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.Workers,
		MaxConnsPerHost:       cfg.Workers * 2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: attemptTimeout,
		ExpectContinueTimeout: 1 * time.Second,

		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
}

func newRobotsHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > MaxRobotsRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func safeDialContext(dialer *net.Dialer, allowPrivateTargets bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split dial address %q: %w", address, err)
		}

		ips, err := resolveDialIPs(ctx, network, host)
		if err != nil {
			return nil, err
		}
		if err := validateResolvedTargetIPs(host, ips, allowPrivateTargets); err != nil {
			return nil, err
		}

		var lastErr error
		for _, ip := range ips {
			if !ipMatchesNetwork(ip, network) {
				continue
			}

			dialAddress := net.JoinHostPort(ip.String(), port)
			conn, err := dialer.DialContext(ctx, network, dialAddress)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}

		if lastErr != nil {
			return nil, fmt.Errorf("dial %s: %w", address, lastErr)
		}
		return nil, fmt.Errorf("no resolved IP address for %s matches network %s", address, network)
	}
}

func resolveDialIPs(ctx context.Context, network, host string) ([]netip.Addr, error) {
	normalizedHost := strings.Trim(strings.TrimSpace(strings.TrimSuffix(host, ".")), "[]")
	if normalizedHost == "" {
		return nil, fmt.Errorf("empty dial host")
	}

	if ip, err := netip.ParseAddr(normalizedHost); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}

	lookupNetwork := "ip"
	if strings.HasSuffix(network, "4") {
		lookupNetwork = "ip4"
	} else if strings.HasSuffix(network, "6") {
		lookupNetwork = "ip6"
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, lookupNetwork, normalizedHost)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no IP addresses returned", normalizedHost)
	}

	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	return ips, nil
}

func validateResolvedTargetIPs(host string, ips []netip.Addr, allowPrivateTargets bool) error {
	if len(ips) == 0 {
		return fmt.Errorf("resolve %s: no IP addresses returned", host)
	}
	if allowPrivateTargets {
		return nil
	}

	for _, ip := range ips {
		if isBlockedTargetIP(ip) {
			return fmt.Errorf("private or local network target is disabled: %s resolved to %s", host, ip)
		}
	}

	return nil
}

func ipMatchesNetwork(ip netip.Addr, network string) bool {
	ip = ip.Unmap()
	if strings.HasSuffix(network, "4") {
		return ip.Is4()
	}
	if strings.HasSuffix(network, "6") {
		return ip.Is6()
	}
	return true
}

func fetchTargetURLBatch(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	cfg Config,
	afterID int64,
	highWatermark int64,
	limit int,
) ([]targetURLRecord, error) {
	dbFetchCtx, fetchCancel := context.WithTimeout(ctx, cfg.DBFetchTimeout)
	defer fetchCancel()

	var records []targetURLRecord
	err := retryDBOperation(
		dbFetchCtx,
		"fetch_target_url_batch",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			rows, err := dbPool.Query(
				dbFetchCtx,
				`SELECT target_id, request_url
				 FROM audit_run_targets
				 WHERE run_id = $1 AND target_id > $2 AND target_id <= $3
				 ORDER BY target_id
				 LIMIT $4`,
				cfg.RunID,
				afterID,
				highWatermark,
				limit,
			)
			if err != nil {
				return err
			}
			defer rows.Close()

			attemptRecords := make([]targetURLRecord, 0, limit)
			for rows.Next() {
				var record targetURLRecord
				if err := rows.Scan(&record.ID, &record.URL); err != nil {
					return fmt.Errorf("scan target URL batch: %w", err)
				}
				attemptRecords = append(attemptRecords, record)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			records = attemptRecords
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("query target URL batch after id %d: %w", afterID, err)
	}

	return records, nil
}

func streamTargetURLs(
	ctx context.Context,
	highWatermark int64,
	batchSize int,
	cfg Config,
	jobs chan<- AuditTarget,
	results chan<- Result,
	fetchBatch targetURLBatchFetcher,
) urlStreamSummary {
	summary := urlStreamSummary{}
	if batchSize <= 0 {
		summary.Error = fmt.Errorf("target URL batch size must be positive")
		return summary
	}
	var afterID int64

	for afterID < highWatermark {
		batch, err := fetchBatch(ctx, afterID, highWatermark, batchSize)
		if err != nil {
			summary.Error = err
			return summary
		}
		if len(batch) == 0 {
			break
		}

		for _, record := range batch {
			if record.ID <= afterID {
				summary.Error = fmt.Errorf(
					"target URL batch returned non-increasing id %d after %d",
					record.ID,
					afterID,
				)
				return summary
			}
			afterID = record.ID

			normalizedURL, err := normalizeTargetURL(record.URL, cfg.AllowPrivateTargets)
			if err != nil {
				target := newAuditTarget(record, record.URL, cfg.TargetFingerprintKey)
				wrappedErr := fmt.Errorf("target %d has invalid URL %s: %s", record.ID, target.SafeURL, sanitizeError(err))
				result := failedScanResult(SEOData{
					URL:           record.URL,
					RobotsAllowed: false,
					RobotsOutcome: robotsOutcomeNotChecked,
				}, errorCodeInvalidTargetURL, wrappedErr)
				result.Target = target
				slog.Warn("URL не пройшов валідацію", "target_id", record.ID, "url", target.SafeURL, "error", sanitizeError(err))
				select {
				case <-ctx.Done():
					summary.Error = ctx.Err()
					return summary
				case results <- result:
					summary.Skipped++
				}
				continue
			}
			target := newAuditTarget(record, normalizedURL, cfg.TargetFingerprintKey)

			select {
			case <-ctx.Done():
				summary.Error = ctx.Err()
				return summary
			case jobs <- target:
				summary.Queued++
			}
		}
	}

	return summary
}

func saveResults(ctx context.Context, dbPool *pgxpool.Pool, results <-chan Result, cfg Config) ResultSummary {
	query := `
		INSERT INTO audit_results (
			run_id, target_id, safe_url, target_fingerprint, fingerprint_key_id, status_code, scan_status, error_code, error_message,
			is_redirect, redirect_url, title, title_status, description, description_status,
			h1, h1_count, h2_to_h6_status, og_title, og_description, og_image, twitter_card,
			internal_links_count, external_links_count, links_count, canonical_url, is_self_canonical,
			meta_robots, x_robots_tag, robots_allowed, robots_outcome, has_json_ld, has_viewport,
			total_images, images_missing_alt, word_count, duration_ms,
			safe_url_truncated, safe_url_original_length,
			redirect_url_truncated, redirect_url_original_length,
			title_truncated, title_original_length,
			h1_truncated, h1_original_length,
			og_title_truncated, og_title_original_length,
			og_image_truncated, og_image_original_length,
			twitter_card_truncated, twitter_card_original_length,
			canonical_url_truncated, canonical_url_original_length,
			meta_robots_truncated, meta_robots_original_length,
			x_robots_tag_truncated, x_robots_tag_original_length
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57)
		ON CONFLICT (run_id, target_id) DO UPDATE SET
			safe_url = EXCLUDED.safe_url,
			target_fingerprint = EXCLUDED.target_fingerprint,
			fingerprint_key_id = EXCLUDED.fingerprint_key_id,
			status_code = EXCLUDED.status_code,
			scan_status = EXCLUDED.scan_status,
			error_code = EXCLUDED.error_code,
			error_message = EXCLUDED.error_message,
			is_redirect = EXCLUDED.is_redirect,
			redirect_url = EXCLUDED.redirect_url,
			title = EXCLUDED.title,
			title_status = EXCLUDED.title_status,
			description = EXCLUDED.description,
			description_status = EXCLUDED.description_status,
			h1 = EXCLUDED.h1,
			h1_count = EXCLUDED.h1_count,
			h2_to_h6_status = EXCLUDED.h2_to_h6_status,
			og_title = EXCLUDED.og_title,
			og_description = EXCLUDED.og_description,
			og_image = EXCLUDED.og_image,
			twitter_card = EXCLUDED.twitter_card,
			internal_links_count = EXCLUDED.internal_links_count,
			external_links_count = EXCLUDED.external_links_count,
			links_count = EXCLUDED.links_count,
			canonical_url = EXCLUDED.canonical_url,
			is_self_canonical = EXCLUDED.is_self_canonical,
			meta_robots = EXCLUDED.meta_robots,
			x_robots_tag = EXCLUDED.x_robots_tag,
			robots_allowed = EXCLUDED.robots_allowed,
			robots_outcome = EXCLUDED.robots_outcome,
			has_json_ld = EXCLUDED.has_json_ld,
			has_viewport = EXCLUDED.has_viewport,
			total_images = EXCLUDED.total_images,
			images_missing_alt = EXCLUDED.images_missing_alt,
			word_count = EXCLUDED.word_count,
			duration_ms = EXCLUDED.duration_ms,
			safe_url_truncated = EXCLUDED.safe_url_truncated,
			safe_url_original_length = EXCLUDED.safe_url_original_length,
			redirect_url_truncated = EXCLUDED.redirect_url_truncated,
			redirect_url_original_length = EXCLUDED.redirect_url_original_length,
			title_truncated = EXCLUDED.title_truncated,
			title_original_length = EXCLUDED.title_original_length,
			h1_truncated = EXCLUDED.h1_truncated,
			h1_original_length = EXCLUDED.h1_original_length,
			og_title_truncated = EXCLUDED.og_title_truncated,
			og_title_original_length = EXCLUDED.og_title_original_length,
			og_image_truncated = EXCLUDED.og_image_truncated,
			og_image_original_length = EXCLUDED.og_image_original_length,
			twitter_card_truncated = EXCLUDED.twitter_card_truncated,
			twitter_card_original_length = EXCLUDED.twitter_card_original_length,
			canonical_url_truncated = EXCLUDED.canonical_url_truncated,
			canonical_url_original_length = EXCLUDED.canonical_url_original_length,
			meta_robots_truncated = EXCLUDED.meta_robots_truncated,
			meta_robots_original_length = EXCLUDED.meta_robots_original_length,
			x_robots_tag_truncated = EXCLUDED.x_robots_tag_truncated,
			x_robots_tag_original_length = EXCLUDED.x_robots_tag_original_length,
			created_at = CURRENT_TIMESTAMP;`

	summary := ResultSummary{}
	for res := range results {
		summary.Received++
		d := res.Data
		target := res.Target
		if target.TargetID == 0 {
			slog.Error("Результат SEO-аудиту не має зв'язку зі snapshot target", "url", redactURL(d.URL))
			summary.Failed++
			continue
		}
		if target.SafeURL == "" || len(target.Fingerprint) == 0 {
			target = newAuditTarget(targetURLRecord{ID: target.TargetID, URL: d.URL}, d.URL, cfg.TargetFingerprintKey)
		}
		resultFailed := res.Error != nil || d.ScanStatus == scanStatusFailed
		if resultFailed {
			if d.ScanStatus == "" {
				d.ScanStatus = scanStatusFailed
			}
			if d.ErrorCode == "" {
				d.ErrorCode = errorCodeInternal
			}
			if d.ErrorMessage == "" && res.Error != nil {
				d.ErrorMessage = sanitizeError(res.Error)
			}
			d = sanitizeSEODataForStorage(d)
			if res.Error != nil {
				slog.Error(
					"Задача завершилася помилкою",
					"target_id",
					target.TargetID,
					"url",
					target.SafeURL,
					"error_code",
					d.ErrorCode,
					"error",
					d.ErrorMessage,
				)
			}
			summary.Failed++
		}
		if d.ScanStatus == "" {
			d.ScanStatus = scanStatusCompleted
		}
		if d.RobotsOutcome == "" {
			d.RobotsOutcome = robotsOutcomeNotChecked
		}
		d.URL = target.SafeURL
		d = sanitizeSEODataForStorage(d)
		fingerprintKeyID := cfg.TargetFingerprintKeyID
		if fingerprintKeyID == "" {
			fingerprintKeyID = DefaultTargetFingerprintKeyID
		}

		dbWriteCtx, writeCancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
		err := retryDBOperation(
			dbWriteCtx,
			"save_audit_result",
			retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
			func() error {
				tx, err := dbPool.Begin(dbWriteCtx)
				if err != nil {
					return err
				}
				defer func() {
					_ = tx.Rollback(dbWriteCtx)
				}()

				if _, err := tx.Exec(
					dbWriteCtx,
					query,
					cfg.RunID,
					target.TargetID,
					d.URL,
					target.Fingerprint,
					fingerprintKeyID,
					d.StatusCode,
					d.ScanStatus,
					d.ErrorCode,
					d.ErrorMessage,
					d.IsRedirect,
					d.RedirectURL,
					d.Title,
					d.TitleStatus,
					d.Description,
					d.DescriptionStatus,
					d.H1,
					d.H1Count,
					d.H2ToH6Status,
					d.OGTitle,
					d.OGDescription,
					d.OGImage,
					d.TwitterCard,
					d.InternalLinksCount,
					d.ExternalLinksCount,
					d.LinksCount,
					d.CanonicalURL,
					d.IsSelfCanonical,
					d.MetaRobots,
					d.XRobotsTag,
					d.RobotsAllowed,
					d.RobotsOutcome,
					d.HasJsonLd,
					d.HasViewport,
					d.TotalImages,
					d.ImagesMissingAlt,
					d.WordCount,
					d.Duration.Milliseconds(),
					d.SafeURLTruncated,
					d.SafeURLOriginalLength,
					d.RedirectURLTruncated,
					d.RedirectURLOriginalLength,
					d.TitleTruncated,
					d.TitleOriginalLength,
					d.H1Truncated,
					d.H1OriginalLength,
					d.OGTitleTruncated,
					d.OGTitleOriginalLength,
					d.OGImageTruncated,
					d.OGImageOriginalLength,
					d.TwitterCardTruncated,
					d.TwitterCardOriginalLength,
					d.CanonicalURLTruncated,
					d.CanonicalURLOriginalLength,
					d.MetaRobotsTruncated,
					d.MetaRobotsOriginalLength,
					d.XRobotsTagTruncated,
					d.XRobotsTagOriginalLength,
				); err != nil {
					return err
				}
				if err := markAuditRunTargetFinished(
					dbWriteCtx,
					tx,
					cfg.RunID,
					target.TargetID,
					finalAuditTargetStatus(d, resultFailed),
					d.ErrorMessage,
				); err != nil {
					return err
				}
				return tx.Commit(dbWriteCtx)
			},
		)
		writeCancel()

		if err != nil {
			slog.Error("Не вдалося зберегти результат SEO-аудиту", "target_id", target.TargetID, "url", d.URL, "error", sanitizeError(err))
			if !resultFailed {
				summary.Failed++
			}
			continue
		}

		summary.Saved++
		if !resultFailed {
			summary.Successful++
		}
		slog.Debug("Результат SEO-аудиту збережено", "target_id", target.TargetID, "url", d.URL)
	}

	return summary
}

func worker(
	schedulingCtx context.Context,
	operationCtx context.Context,
	id int,
	jobs <-chan AuditTarget,
	results chan<- Result,
	pageClient *http.Client,
	robotsClient *http.Client,
	robotsCache *robotsPolicyCache,
	dbPool *pgxpool.Pool,
	cfg Config,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	workerLogger := slog.With("worker_id", id)

	for {
		var target AuditTarget
		select {
		case <-schedulingCtx.Done():
			workerLogger.Debug("Worker не приймає нові задачі після сигналу зупинки")
			return
		case queuedTarget, ok := <-jobs:
			if !ok {
				return
			}
			target = queuedTarget
		}
		if schedulingCtx.Err() != nil {
			workerLogger.Debug("Worker залишає заплановану задачу для наступного запуску")
			return
		}

		if err := markAuditRunTargetStarted(operationCtx, dbPool, target, cfg); err != nil {
			workerLogger.Warn("Не вдалося позначити target як running", "target_id", target.TargetID, "url", target.SafeURL, "error", err)
		}
		start := time.Now()

		allowed, err := robotsCache.isAllowedByRobots(operationCtx, robotsClient, target.RequestURL, cfg.RobotsTotalTimeout)
		if err != nil {
			wrappedErr := fmt.Errorf("worker %d cannot verify robots.txt for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(SEOData{
				URL:           target.RequestURL,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeUnavailable,
				Duration:      time.Since(start),
			}, errorCodeRobotsUnavailable, wrappedErr)
			result.Target = target
			results <- result
			continue
		}
		if !allowed {
			workerLogger.Warn("Сканування URL заборонено правилами robots.txt", "target_id", target.TargetID, "url", target.SafeURL)
			results <- Result{Target: target, Data: SEOData{
				URL:           target.RequestURL,
				ScanStatus:    scanStatusBlockedByRobots,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeDisallowed,
				Duration:      time.Since(start),
			}}
			continue
		}
		baseData := SEOData{
			URL:           target.RequestURL,
			RobotsAllowed: true,
			RobotsOutcome: robotsOutcomeAllowed,
		}

		reqCtx, reqCancel := context.WithTimeout(operationCtx, cfg.HTTPTotalTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.RequestURL, nil)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot create request for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeRequestCreationFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}
		req.Header.Set("User-Agent", UserAgentStr)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")

		resp, err := pageClient.Do(req)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d network request failed for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeRequestFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			redirectURL := resp.Header.Get("Location")
			safeRedirectURL := redactURL(redirectURL)
			resp.Body.Close()
			reqCancel()

			workerLogger.Info(
				"Виявлено HTTP-редирект",
				"from",
				target.SafeURL,
				"to",
				safeRedirectURL,
				"status",
				resp.StatusCode,
			)

			results <- Result{Target: target, Data: SEOData{
				URL:           target.RequestURL,
				StatusCode:    httpStatus(resp.StatusCode),
				ScanStatus:    scanStatusRedirect,
				IsRedirect:    true,
				RedirectURL:   redirectURL,
				RobotsAllowed: true,
				RobotsOutcome: robotsOutcomeAllowed,
				Duration:      time.Since(start),
			}}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			data := SEOData{
				URL:           target.RequestURL,
				StatusCode:    httpStatus(resp.StatusCode),
				ScanStatus:    scanStatusCompleted,
				XRobotsTag:    strings.TrimSpace(resp.Header.Get("X-Robots-Tag")),
				RobotsAllowed: true,
				RobotsOutcome: robotsOutcomeAllowed,
				Duration:      time.Since(start),
			}
			resp.Body.Close()
			reqCancel()
			workerLogger.Info("Збережено HTTP-статус без HTML-парсингу", "target_id", target.TargetID, "url", target.SafeURL, "status", resp.StatusCode)
			results <- Result{Target: target, Data: data}
			continue
		}

		baseData.StatusCode = httpStatus(resp.StatusCode)
		baseData.XRobotsTag = strings.TrimSpace(resp.Header.Get("X-Robots-Tag"))
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			resp.Body.Close()
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d rejected %s: missing Content-Type header", id, target.SafeURL)
			result := failedScanResult(baseData, errorCodeMissingContentType, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		if err := validateHTMLContentType(contentType); err != nil {
			resp.Body.Close()
			reqCancel()
			workerLogger.Warn("Пропущено непідтримуваний тип контенту", "target_id", target.TargetID, "url", target.SafeURL, "content_type", contentType)
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d skipped unsupported content type %q for %s: %s", id, contentType, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeUnsupportedContentType, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		data, err := parsePage(resp, target.RequestURL, cfg.MaxHTMLBodyBytes)
		resp.Body.Close()
		reqCancel()

		if err != nil {
			data.RobotsAllowed = true
			data.RobotsOutcome = robotsOutcomeAllowed
			data.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot parse HTML for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(data, errorCodeResponseParseFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		data.ScanStatus = scanStatusCompleted
		data.RobotsAllowed = true
		data.RobotsOutcome = robotsOutcomeAllowed
		data.Duration = time.Since(start)

		select {
		case <-operationCtx.Done():
			workerLogger.Warn("Відправлення результату скасовано після вичерпання shutdown timeout", "target_id", target.TargetID, "url", target.SafeURL)
			return
		case results <- Result{Target: target, Data: data}:
		}
	}
}

func normalizeTargetURL(rawURL string, allowPrivateTargets bool) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty URL")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("credentials in URL are not allowed")
	}
	if !allowPrivateTargets && isPrivateHost(parsed.Host) {
		return "", fmt.Errorf("private or local targets are disabled")
	}
	parsed.Fragment = ""

	return parsed.String(), nil
}

func isPrivateHost(host string) bool {
	hostOnly := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = parsedHost
	}

	hostOnly = strings.Trim(strings.ToLower(strings.TrimSuffix(hostOnly, ".")), "[]")
	if hostOnly == "localhost" || strings.HasSuffix(hostOnly, ".localhost") {
		return true
	}

	ip, err := netip.ParseAddr(hostOnly)
	if err != nil {
		return false
	}

	return isBlockedTargetIP(ip)
}

func isBlockedTargetIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}

	for _, prefix := range blockedTargetIPPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}

	return false
}

func robotsRequestPath(parsed *url.URL) string {
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return path
}

func isPathAllowedByRobots(content, userAgent, requestPath string) bool {
	groups := parseRobotsGroups(content)
	group, ok := selectRobotsGroup(groups, userAgent)
	if !ok || len(group.rules) == 0 {
		return true
	}

	allowed := true
	bestSpecificity := -1
	for _, rule := range group.rules {
		if !robotsPatternMatches(rule.pattern, requestPath) {
			continue
		}
		if rule.specificity > bestSpecificity || (rule.specificity == bestSpecificity && rule.allow) {
			allowed = rule.allow
			bestSpecificity = rule.specificity
		}
	}

	return allowed
}

func parseRobotsGroups(content string) []robotsGroup {
	var groups []robotsGroup
	var current robotsGroup
	seenRule := false

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(stripRobotsComment(line))
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if len(current.agents) > 0 && seenRule {
				groups = append(groups, current)
				current = robotsGroup{}
				seenRule = false
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "allow", "disallow":
			if len(current.agents) == 0 {
				continue
			}
			seenRule = true
			if key == "disallow" && value == "" {
				continue
			}
			current.rules = append(current.rules, robotsRule{
				allow:       key == "allow",
				pattern:     value,
				specificity: robotsRuleSpecificity(value),
			})
		}
	}

	if len(current.agents) > 0 {
		groups = append(groups, current)
	}

	return groups
}

func stripRobotsComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

func selectRobotsGroup(groups []robotsGroup, userAgent string) (robotsGroup, bool) {
	bestLength := -1
	var selected robotsGroup
	found := false

	for _, group := range groups {
		groupBestLength := -1
		for _, agent := range group.agents {
			if robotsAgentMatches(agent, userAgent) && len(agent) > groupBestLength {
				groupBestLength = len(agent)
			}
		}

		if groupBestLength < 0 {
			continue
		}
		if groupBestLength > bestLength {
			selected = group
			bestLength = groupBestLength
			found = true
			continue
		}
		if groupBestLength == bestLength {
			selected.rules = append(selected.rules, group.rules...)
		}
	}

	return selected, found
}

func robotsAgentMatches(agent, userAgent string) bool {
	agent = strings.TrimSpace(strings.ToLower(agent))
	if agent == "" {
		return false
	}
	if agent == "*" {
		return true
	}

	ua := strings.ToLower(userAgent)
	product := strings.SplitN(ua, "/", 2)[0]
	return strings.Contains(ua, agent) || strings.Contains(product, agent)
}

func robotsRuleSpecificity(pattern string) int {
	cleaned := strings.NewReplacer("*", "", "$", "").Replace(pattern)
	return len(cleaned)
}

func robotsPatternMatches(pattern, requestPath string) bool {
	if pattern == "" {
		return false
	}

	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}

	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
	if endAnchored {
		expr += "$"
	}

	matched, err := regexp.MatchString(expr, requestPath)
	return err == nil && matched
}

func parsePage(resp *http.Response, targetURL string, maxBodyBytes int64) (SEOData, error) {
	data := SEOData{
		URL:        targetURL,
		StatusCode: httpStatus(resp.StatusCode),
		XRobotsTag: strings.TrimSpace(resp.Header.Get("X-Robots-Tag")),
	}

	if resp.StatusCode != http.StatusOK {
		return data, nil
	}

	body, err := readLimited(resp.Body, maxBodyBytes)
	if err != nil {
		return data, err
	}

	decodedBody, err := charset.NewReader(bytes.NewReader(body), resp.Header.Get("Content-Type"))
	if err != nil {
		return data, fmt.Errorf("decode HTML charset: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(decodedBody)
	if err != nil {
		return data, fmt.Errorf("parse HTML: %w", err)
	}

	data.Title = strings.TrimSpace(doc.Find("title").First().Text())
	titleLen := utf8.RuneCountInString(data.Title)
	if titleLen == 0 {
		data.TitleStatus = "Missing"
	} else if titleLen < MinTitleLen {
		data.TitleStatus = "Too Short"
	} else if titleLen > MaxTitleLen {
		data.TitleStatus = "Too Long"
	} else {
		data.TitleStatus = "OK"
	}

	data.Description = strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	descLen := utf8.RuneCountInString(data.Description)
	if descLen == 0 {
		data.DescriptionStatus = "Missing"
	} else if descLen < MinDescriptionLen {
		data.DescriptionStatus = "Too Short"
	} else if descLen > MaxDescriptionLen {
		data.DescriptionStatus = "Too Long"
	} else {
		data.DescriptionStatus = "OK"
	}

	h1Selection := doc.Find("h1")
	data.H1Count = h1Selection.Length()
	if data.H1Count > 0 {
		data.H1 = strings.TrimSpace(h1Selection.First().Text())
	} else {
		data.H1 = "[Missing H1]"
	}

	var subHeaders []string
	for i := 2; i <= 6; i++ {
		count := doc.Find(fmt.Sprintf("h%d", i)).Length()
		if count > 0 {
			subHeaders = append(subHeaders, fmt.Sprintf("H%d:%d", i, count))
		}
	}
	if len(subHeaders) > 0 {
		data.H2ToH6Status = strings.Join(subHeaders, ", ")
	} else {
		data.H2ToH6Status = "No sub-headers (H2-H6)"
	}

	data.OGTitle = strings.TrimSpace(doc.Find("meta[property='og:title']").AttrOr("content", ""))
	data.OGDescription = strings.TrimSpace(doc.Find("meta[property='og:description']").AttrOr("content", ""))
	data.OGImage = strings.TrimSpace(doc.Find("meta[property='og:image']").AttrOr("content", ""))
	data.TwitterCard = strings.TrimSpace(doc.Find("meta[name='twitter:card']").AttrOr("content", ""))

	baseParsed, _ := url.Parse(targetURL)
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}
		href = strings.TrimSpace(href)
		hrefLower := strings.ToLower(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(hrefLower, "javascript:") || strings.HasPrefix(hrefLower, "mailto:") || strings.HasPrefix(hrefLower, "tel:") {
			return
		}

		linkParsed, err := url.Parse(href)
		if err != nil {
			return
		}

		if linkParsed.Host == "" || strings.EqualFold(linkParsed.Host, baseParsed.Host) {
			data.InternalLinksCount++
		} else {
			data.ExternalLinksCount++
		}
	})
	data.LinksCount = data.InternalLinksCount + data.ExternalLinksCount

	data.CanonicalURL = strings.TrimSpace(doc.Find("link[rel='canonical']").AttrOr("href", ""))
	data.IsSelfCanonical = isSelfCanonical(data.CanonicalURL, targetURL)
	data.MetaRobots = strings.TrimSpace(doc.Find("meta[name='robots']").AttrOr("content", ""))

	data.HasJsonLd = doc.Find("script[type='application/ld+json']").Length() > 0
	data.HasViewport = doc.Find("meta[name='viewport']").Length() > 0

	imgSelection := doc.Find("img")
	data.TotalImages = imgSelection.Length()
	imgSelection.Each(func(i int, s *goquery.Selection) {
		alt, exists := s.Attr("alt")
		if !exists || strings.TrimSpace(alt) == "" {
			data.ImagesMissingAlt++
		}
	})

	bodyText := doc.Find("body").Text()
	words := strings.Fields(bodyText)
	data.WordCount = len(words)

	return data, nil
}

func validateHTMLContentType(raw string) error {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("parse Content-Type: %w", err)
	}
	switch strings.ToLower(mediaType) {
	case "text/html", "application/xhtml+xml":
		return nil
	default:
		return fmt.Errorf("media type %q is not HTML", mediaType)
	}
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive")
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds configured limit of %d bytes", maxBytes)
	}

	return body, nil
}

func isSelfCanonical(canonicalURL, targetURL string) bool {
	if strings.TrimSpace(canonicalURL) == "" {
		return false
	}

	targetParsed, err := url.Parse(targetURL)
	if err != nil {
		return false
	}

	canonicalParsed, err := url.Parse(canonicalURL)
	if err != nil {
		return false
	}
	if !canonicalParsed.IsAbs() {
		canonicalParsed = targetParsed.ResolveReference(canonicalParsed)
	}

	normalize := func(parsed *url.URL) string {
		copyValue := *parsed
		copyValue.Scheme = strings.ToLower(copyValue.Scheme)
		copyValue.Host = strings.ToLower(copyValue.Host)
		copyValue.Fragment = ""
		return strings.TrimRight(copyValue.String(), "/")
	}

	return normalize(canonicalParsed) == normalize(targetParsed)
}
