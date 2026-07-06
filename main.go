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
	exitOK       = 0
	exitCritical = 1
	exitPartial  = 2
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
	URL                string
	StatusCode         *int
	ScanStatus         string
	ErrorCode          string
	ErrorMessage       string
	IsRedirect         bool
	RedirectURL        string
	Title              string
	TitleStatus        string
	Description        string
	DescriptionStatus  string
	H1                 string
	H1Count            int
	H2ToH6Status       string
	OGTitle            string
	OGDescription      string
	OGImage            string
	TwitterCard        string
	InternalLinksCount int
	ExternalLinksCount int
	LinksCount         int
	CanonicalURL       string
	IsSelfCanonical    bool
	MetaRobots         string
	XRobotsTag         string
	RobotsAllowed      bool
	RobotsOutcome      string
	HasJsonLd          bool
	HasViewport        bool
	TotalImages        int
	ImagesMissingAlt   int
	WordCount          int
	Duration           time.Duration
}

type Result struct {
	Data  SEOData
	Error error
}

type ResultSummary struct {
	Received int
	Saved    int
	Failed   int
}

func httpStatus(code int) *int {
	return &code
}

func failedScanResult(data SEOData, code string, err error) Result {
	data.ScanStatus = scanStatusFailed
	data.ErrorCode = code
	data.ErrorMessage = err.Error()
	return Result{Data: data, Error: err}
}

type targetURLRecord struct {
	ID  int64
	URL string
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

func run() int {
	bootstrapLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(bootstrapLogger)
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("Некоректна конфігурація рантайму", "error", err)
		return exitCritical
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).With("run_id", cfg.RunID)
	slog.SetDefault(logger)

	slog.Info("Запускається етичний SEO-аудитор")

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info(
		"Конфігурацію рантайму ініціалізовано",
		"workers",
		cfg.Workers,
		"request_timeout",
		cfg.HTTPRequestTimeout.String(),
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
		return exitCritical
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
		return exitCritical
	}

	dbRetryPolicy := retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay}
	if err := retryDBOperation(dbInitCtx, "ping", dbRetryPolicy, func() error {
		return dbPool.Ping(dbInitCtx)
	}); err != nil {
		dbCancel()
		dbPool.Close()
		slog.Error("PostgreSQL недоступний під час перевірки підключення", "error", err)
		return exitCritical
	}
	dbCancel()
	slog.Info("Підключення до PostgreSQL підтверджено", "max_conns", poolConfig.MaxConns)

	defer func() {
		slog.Info("Закривається пул підключень PostgreSQL")
		dbPool.Close()
	}()

	queueSnapshot, err := fetchTargetURLSnapshot(signalCtx, dbPool, cfg)
	if err != nil {
		if signalCtx.Err() != nil {
			slog.Warn("Запуск перервано до формування черги URL", "error", signalCtx.Err())
			return exitPartial
		}
		slog.Error("Не вдалося зафіксувати верхню межу черги URL", "error", err)
		return exitCritical
	}
	if queueSnapshot.Total == 0 {
		slog.Warn("Черга URL порожня")
		if signalCtx.Err() != nil {
			return exitPartial
		}
		return exitOK
	}
	slog.Info(
		"Зафіксовано знімок черги URL",
		"high_watermark",
		queueSnapshot.HighWatermark,
		"total_urls",
		queueSnapshot.Total,
		"batch_size",
		cfg.URLBatchSize,
	)

	queueCapacity := cfg.Workers * QueueBufferPerWorker
	jobs := make(chan string, queueCapacity)
	results := make(chan Result, queueCapacity)

	customTransport := newHTTPTransport(cfg)
	hostPolicies := newHostPolicyManager(
		cfg.RateLimitInterval,
		cfg.MaxConcurrentPerHost,
		DefaultHostStateCacheSize,
		MaxRetryAfterDelay,
	)
	politeTransport := &politeRoundTripper{
		base:     customTransport,
		policies: hostPolicies,
	}
	retryingTransport := &retryRoundTripper{
		base: politeTransport,
		policy: retryPolicy{
			maxRetries: cfg.HTTPMaxRetries,
			baseDelay:  cfg.RetryBaseDelay,
			maxDelay:   cfg.RetryMaxDelay,
		},
	}
	robotsCache := newRobotsPolicyCache(cfg.RobotsCacheTTL, DefaultRobotsCacheMaxEntries)

	pageHTTPClient := &http.Client{
		Transport: retryingTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	robotsHTTPClient := newRobotsHTTPClient(retryingTransport)
	defer customTransport.CloseIdleConnections()

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
		go worker(signalCtx, operationCtx, w, jobs, results, pageHTTPClient, robotsHTTPClient, robotsCache, cfg, &wg)
	}

	streamDone := make(chan urlStreamSummary, 1)
	go func() {
		defer close(jobs)
		streamDone <- streamTargetURLs(
			signalCtx,
			queueSnapshot.HighWatermark,
			cfg.URLBatchSize,
			cfg.AllowPrivateTargets,
			jobs,
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
	if streamSummary.Error != nil && !streamCanceledByShutdown {
		slog.Error(
			"Потокове читання черги URL завершилося помилкою",
			"error",
			streamSummary.Error,
			"queued_urls",
			streamSummary.Queued,
			"saved_results",
			summary.Saved,
		)
		return exitCritical
	}

	deferredURLs := queueSnapshot.Total - int64(streamSummary.Queued+streamSummary.Skipped)
	if deferredURLs < 0 {
		deferredURLs = 0
	}
	missingResults := streamSummary.Queued - summary.Received
	if missingResults < 0 {
		missingResults = 0
	}
	if streamSummary.Queued == 0 {
		slog.Warn("Черга URL не містить валідних цілей", "skipped_urls", streamSummary.Skipped)
	}
	if shutdownRequested || deferredURLs > 0 || streamSummary.Skipped > 0 || summary.Failed > 0 || missingResults > 0 {
		slog.Error(
			"Роботу парсера завершено з частковими помилками",
			"shutdown_requested",
			shutdownRequested,
			"deferred_urls",
			deferredURLs,
			"skipped_urls",
			streamSummary.Skipped,
			"failed_results",
			summary.Failed,
			"missing_results",
			missingResults,
			"saved_results",
			summary.Saved,
		)
		return exitPartial
	}

	slog.Info(
		"Роботу парсера завершено",
		"queued_urls",
		streamSummary.Queued,
		"saved_results",
		summary.Saved,
	)
	return exitOK
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

func newHTTPTransport(cfg Config) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   cfg.HTTPRequestTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext:           safeDialContext(dialer, cfg.AllowPrivateTargets),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.Workers,
		MaxConnsPerHost:       cfg.Workers * 2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: cfg.HTTPRequestTimeout,
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

func fetchTargetURLSnapshot(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (targetURLSnapshot, error) {
	dbFetchCtx, fetchCancel := context.WithTimeout(ctx, cfg.DBFetchTimeout)
	defer fetchCancel()

	var snapshot targetURLSnapshot
	err := retryDBOperation(
		dbFetchCtx,
		"fetch_target_url_snapshot",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			return dbPool.QueryRow(
				dbFetchCtx,
				"SELECT COALESCE(MAX(id), 0), COUNT(*) FROM pages_to_scan WHERE is_active = TRUE",
			).Scan(&snapshot.HighWatermark, &snapshot.Total)
		},
	)
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("query target URL snapshot: %w", err)
	}

	return snapshot, nil
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
				`SELECT id, url
				 FROM pages_to_scan
				 WHERE is_active = TRUE AND id > $1 AND id <= $2
				 ORDER BY id
				 LIMIT $3`,
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
	allowPrivateTargets bool,
	jobs chan<- string,
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

			normalizedURL, err := normalizeTargetURL(record.URL, allowPrivateTargets)
			if err != nil {
				slog.Warn("URL пропущено через помилку валідації", "url", record.URL, "error", err)
				summary.Skipped++
				continue
			}

			select {
			case <-ctx.Done():
				summary.Error = ctx.Err()
				return summary
			case jobs <- normalizedURL:
				summary.Queued++
			}
		}
	}

	return summary
}

func saveResults(ctx context.Context, dbPool *pgxpool.Pool, results <-chan Result, cfg Config) ResultSummary {
	query := `
		INSERT INTO seo_results (
			url, status_code, scan_status, error_code, error_message,
			is_redirect, redirect_url, title, title_status, description, description_status,
			h1, h1_count, h2_to_h6_status, og_title, og_description, og_image, twitter_card,
			internal_links_count, external_links_count, links_count, canonical_url, is_self_canonical,
			meta_robots, x_robots_tag, robots_allowed, robots_outcome, has_json_ld, has_viewport,
			total_images, images_missing_alt, word_count, duration_ms, run_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34)
		ON CONFLICT (url) DO UPDATE SET
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
			run_id = EXCLUDED.run_id,
			created_at = CURRENT_TIMESTAMP;`

	summary := ResultSummary{}
	for res := range results {
		summary.Received++
		d := res.Data
		resultFailed := res.Error != nil
		if res.Error != nil {
			if d.ScanStatus == "" {
				d.ScanStatus = scanStatusFailed
			}
			if d.ErrorCode == "" {
				d.ErrorCode = errorCodeInternal
			}
			if d.ErrorMessage == "" {
				d.ErrorMessage = res.Error.Error()
			}
			slog.Error(
				"Задача завершилася помилкою",
				"url",
				d.URL,
				"error_code",
				d.ErrorCode,
				"error",
				res.Error,
			)
			summary.Failed++
		}
		if d.ScanStatus == "" {
			d.ScanStatus = scanStatusCompleted
		}
		if d.RobotsOutcome == "" {
			d.RobotsOutcome = robotsOutcomeNotChecked
		}

		dbWriteCtx, writeCancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
		err := retryDBOperation(
			dbWriteCtx,
			"save_audit_result",
			retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
			func() error {
				_, err := dbPool.Exec(
					dbWriteCtx,
					query,
					d.URL,
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
					cfg.RunID,
				)
				return err
			},
		)
		writeCancel()

		if err != nil {
			slog.Error("Не вдалося зберегти результат SEO-аудиту", "url", d.URL, "error", err)
			if !resultFailed {
				summary.Failed++
			}
			continue
		}

		summary.Saved++
		slog.Debug("Результат SEO-аудиту збережено", "url", d.URL)
	}

	return summary
}

func worker(
	schedulingCtx context.Context,
	operationCtx context.Context,
	id int,
	jobs <-chan string,
	results chan<- Result,
	pageClient *http.Client,
	robotsClient *http.Client,
	robotsCache *robotsPolicyCache,
	cfg Config,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	workerLogger := slog.With("worker_id", id)

	for {
		var targetURL string
		select {
		case <-schedulingCtx.Done():
			workerLogger.Debug("Worker не приймає нові задачі після сигналу зупинки")
			return
		case queuedURL, ok := <-jobs:
			if !ok {
				return
			}
			targetURL = queuedURL
		}
		if schedulingCtx.Err() != nil {
			workerLogger.Debug("Worker залишає заплановану задачу для наступного запуску")
			return
		}

		start := time.Now()

		allowed, err := robotsCache.isAllowedByRobots(operationCtx, robotsClient, targetURL, cfg.RobotsTimeout)
		if err != nil {
			wrappedErr := fmt.Errorf("worker %d cannot verify robots.txt for %s: %w", id, targetURL, err)
			results <- failedScanResult(SEOData{
				URL:           targetURL,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeUnavailable,
				Duration:      time.Since(start),
			}, errorCodeRobotsUnavailable, wrappedErr)
			continue
		}
		if !allowed {
			workerLogger.Warn("Сканування URL заборонено правилами robots.txt", "url", targetURL)
			results <- Result{Data: SEOData{
				URL:           targetURL,
				ScanStatus:    scanStatusBlockedByRobots,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeDisallowed,
				Duration:      time.Since(start),
			}}
			continue
		}
		baseData := SEOData{
			URL:           targetURL,
			RobotsAllowed: true,
			RobotsOutcome: robotsOutcomeAllowed,
		}

		reqCtx, reqCancel := context.WithTimeout(operationCtx, cfg.HTTPRequestTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot create request for %s: %w", id, targetURL, err)
			results <- failedScanResult(baseData, errorCodeRequestCreationFailed, wrappedErr)
			continue
		}
		req.Header.Set("User-Agent", UserAgentStr)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")

		resp, err := pageClient.Do(req)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d network request failed for %s: %w", id, targetURL, err)
			results <- failedScanResult(baseData, errorCodeRequestFailed, wrappedErr)
			continue
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			redirectURL := resp.Header.Get("Location")
			resp.Body.Close()
			reqCancel()

			workerLogger.Info(
				"Виявлено HTTP-редирект",
				"from",
				targetURL,
				"to",
				redirectURL,
				"status",
				resp.StatusCode,
			)

			results <- Result{Data: SEOData{
				URL:           targetURL,
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
				URL:           targetURL,
				StatusCode:    httpStatus(resp.StatusCode),
				ScanStatus:    scanStatusCompleted,
				XRobotsTag:    strings.TrimSpace(resp.Header.Get("X-Robots-Tag")),
				RobotsAllowed: true,
				RobotsOutcome: robotsOutcomeAllowed,
				Duration:      time.Since(start),
			}
			resp.Body.Close()
			reqCancel()
			workerLogger.Info("Збережено HTTP-статус без HTML-парсингу", "url", targetURL, "status", resp.StatusCode)
			results <- Result{Data: data}
			continue
		}

		baseData.StatusCode = httpStatus(resp.StatusCode)
		baseData.XRobotsTag = strings.TrimSpace(resp.Header.Get("X-Robots-Tag"))
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			resp.Body.Close()
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d rejected %s: missing Content-Type header", id, targetURL)
			results <- failedScanResult(baseData, errorCodeMissingContentType, wrappedErr)
			continue
		}

		if err := validateHTMLContentType(contentType); err != nil {
			resp.Body.Close()
			reqCancel()
			workerLogger.Warn("Пропущено непідтримуваний тип контенту", "url", targetURL, "content_type", contentType)
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d skipped unsupported content type %q for %s: %w", id, contentType, targetURL, err)
			results <- failedScanResult(baseData, errorCodeUnsupportedContentType, wrappedErr)
			continue
		}

		data, err := parsePage(resp, targetURL, cfg.MaxHTMLBodyBytes)
		resp.Body.Close()
		reqCancel()

		if err != nil {
			data.RobotsAllowed = true
			data.RobotsOutcome = robotsOutcomeAllowed
			data.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot parse HTML for %s: %w", id, targetURL, err)
			results <- failedScanResult(data, errorCodeResponseParseFailed, wrappedErr)
			continue
		}

		data.ScanStatus = scanStatusCompleted
		data.RobotsAllowed = true
		data.RobotsOutcome = robotsOutcomeAllowed
		data.Duration = time.Since(start)

		select {
		case <-operationCtx.Done():
			workerLogger.Warn("Відправлення результату скасовано після вичерпання shutdown timeout", "url", targetURL)
			return
		case results <- Result{Data: data}:
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
