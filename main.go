package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	UserAgentStr = "Go-SEOParser-Bot/1.0"

	QueueBufferPerWorker = 2
	MaxRobotsRedirects   = 5
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
		"stale_recovery_batch_timeout",
		cfg.StaleRecoveryBatchTimeout.String(),
		"report_export_timeout",
		cfg.ReportExportTimeout.String(),
		"audit_run_heartbeat",
		cfg.AuditRunHeartbeatInterval.String(),
		"heartbeat_failure_threshold",
		cfg.HeartbeatFailureThreshold,
		"stale_run_threshold",
		cfg.StaleRunThreshold.String(),
		"target_lease_duration",
		cfg.TargetLeaseDuration.String(),
		"shutdown_timeout",
		cfg.ShutdownTimeout.String(),
		"finalization_timeout",
		cfg.FinalizationTimeout.String(),
		"stop_grace_period",
		cfg.StopGracePeriod.String(),
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

	abandonedRuns, err := abandonStaleAuditRuns(signalCtx, dbPool, cfg)
	if err != nil {
		dbPool.Close()
		slog.Error("Не вдалося позначити застарілі запуски аудиту як abandoned", "error", err)
		return exitFatal
	}
	if abandonedRuns > 0 {
		slog.Warn("Застарілі running-запуски позначено як abandoned", "count", abandonedRuns)
	}
	clearedRetainedURLs, err := clearRetainedTerminalAuditRunTargetURLs(signalCtx, dbPool, cfg)
	if err != nil {
		dbPool.Close()
		slog.Error("Не вдалося очистити збережені URL завершених запусків", "error", err)
		return exitFatal
	}
	if clearedRetainedURLs > 0 {
		slog.Info("Очищено URL завершених запусків після перерваної фіналізації", "count", clearedRetainedURLs)
	}

	if err := createAuditRun(signalCtx, dbPool, &cfg); err != nil {
		dbPool.Close()
		slog.Error("Не вдалося зареєструвати запуск аудиту", "error", err)
		return exitFatal
	}
	slog.Info(
		"Підключення до PostgreSQL підтверджено, схема актуальна та запуск аудиту зареєстровано",
		"owner_generation",
		cfg.OwnerGeneration,
		"max_conns",
		poolConfig.MaxConns,
	)

	defer func() {
		slog.Info("Закривається пул підключень PostgreSQL")
		dbPool.Close()
	}()
	runCompletion := auditRunCompletion{Status: auditRunStatusFailed}
	workCtx, cancelWork := context.WithCancelCause(signalCtx)
	defer cancelWork(context.Canceled)
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := startAuditRunHeartbeat(heartbeatCtx, dbPool, cfg, func(err error) {
		slog.Error("Втрачено надійний heartbeat запуску; планування нових targets зупиняється", "error", err)
		cancelWork(err)
	})
	defer func() {
		if signalCtx.Err() == nil {
			if cause := context.Cause(workCtx); cause != nil && !errors.Is(cause, context.Canceled) {
				runCompletion.Status = auditRunStatusFailed
				exitCode = exitFatal
			}
		}

		finalizationCtx, cancelFinalization := context.WithTimeout(
			context.Background(),
			cfg.FinalizationTimeout,
		)
		defer cancelFinalization()

		terminalErr := persistAuditRunTerminalState(
			finalizationCtx,
			dbPool,
			cfg.RunID,
			runCompletion,
			cfg,
		)
		stopHeartbeat()
		<-heartbeatDone
		if terminalErr != nil {
			slog.Error("Не вдалося записати terminal status запуску аудиту", "error", terminalErr)
			exitCode = exitFatal
			return
		}
		if _, err := clearAuditRunTargetURLs(finalizationCtx, dbPool, cfg.RunID, cfg); err != nil {
			slog.Error("Не вдалося очистити збережені URL завершеного запуску", "error", err)
			exitCode = exitFatal
		}
		if signalCtx.Err() == nil {
			publishAuditReport(dbPool, cfg)
		} else {
			slog.Info("HTML-звіт пропущено під час завершення за системним сигналом")
		}
	}()

	targetSnapshot, err := captureAuditRunTargets(workCtx, dbPool, cfg)
	if err != nil {
		if signalCtx.Err() != nil {
			runCompletion.Status = auditRunStatusCanceled
			slog.Warn("Запуск скасовано до фіксації стабільного набору цілей", "error", signalCtx.Err())
			return exitCanceled
		}
		if cause := context.Cause(workCtx); cause != nil {
			slog.Error("Heartbeat завершився фатально до фіксації стабільного набору цілей", "error", cause)
			return exitFatal
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
	runCompletion.SuccessfulURLs = int(targetSnapshot.Successful)
	runCompletion.FailedURLs = int(targetSnapshot.Failed)
	slog.Info(
		"Зафіксовано стабільний набір цілей аудиту",
		"high_watermark",
		targetSnapshot.HighWatermark,
		"total_urls",
		targetSnapshot.Total,
		"already_successful",
		targetSnapshot.Successful,
		"already_failed",
		targetSnapshot.Failed,
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
	pageRetryingTransport := &retryRoundTripper{
		base:     pageCustomTransport,
		policies: hostPolicies,
		policy: retryPolicy{
			maxRetries:     cfg.HTTPMaxRetries,
			attemptTimeout: cfg.HTTPAttemptTimeout,
			baseDelay:      cfg.RetryBaseDelay,
			maxDelay:       cfg.RetryMaxDelay,
		},
	}
	robotsRetryingTransport := &retryRoundTripper{
		base:     robotsCustomTransport,
		policies: hostPolicies,
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

	operationCtx, cancelOperations := context.WithCancel(context.WithoutCancel(workCtx))
	defer cancelOperations()
	processingDone := make(chan struct{})
	shutdownGuardDone := guardGracefulShutdown(
		workCtx,
		processingDone,
		cfg.ShutdownTimeout,
		cancelOperations,
	)

	var wg sync.WaitGroup
	for w := 1; w <= cfg.Workers; w++ {
		wg.Add(1)
		go worker(workCtx, operationCtx, w, jobs, results, pageHTTPClient, robotsHTTPClient, robotsCache, dbPool, cfg, &wg)
	}

	streamDone := make(chan urlStreamSummary, 1)
	streamFinished := make(chan struct{})
	go func() {
		defer close(jobs)
		defer close(streamFinished)
		streamDone <- streamTargetURLs(
			workCtx,
			cfg.URLBatchSize,
			cfg,
			jobs,
			results,
			func(ctx context.Context, limit int) ([]targetURLRecord, error) {
				return claimTargetURLBatch(ctx, dbPool, cfg, limit)
			},
		)
	}()

	go func() {
		closeResultsAfterProducers(&wg, streamFinished, results)
	}()

	slog.Info("Починається паралельна обробка URL та збереження результатів")
	summary := saveResults(operationCtx, dbPool, results, cfg)
	streamSummary := <-streamDone
	close(processingDone)
	<-shutdownGuardDone

	shutdownRequested := signalCtx.Err() != nil
	heartbeatFailure := signalCtx.Err() == nil && context.Cause(workCtx) != nil
	streamCanceledByLifecycle := workCtx.Err() != nil && errors.Is(streamSummary.Error, context.Canceled)
	previouslyProcessed := targetSnapshot.Successful + targetSnapshot.Failed
	unprocessedTargets := targetSnapshot.Total - previouslyProcessed - int64(streamSummary.Queued+streamSummary.Skipped)
	if unprocessedTargets < 0 {
		unprocessedTargets = 0
	}
	expectedResults := streamSummary.Queued + streamSummary.Skipped
	missingResults := expectedResults - summary.Received
	if missingResults < 0 {
		missingResults = 0
	}
	runCompletion.SuccessfulURLs = int(targetSnapshot.Successful) + summary.Successful
	runCompletion.FailedURLs = int(targetSnapshot.Failed) + summary.Failed + missingResults
	if streamSummary.Error != nil && !streamCanceledByLifecycle {
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

	if streamSummary.Queued == 0 && previouslyProcessed < targetSnapshot.Total {
		slog.Warn("Стабільний набір цілей не містить валідних URL", "skipped_urls", streamSummary.Skipped)
	}
	if heartbeatFailure {
		runCompletion.Status = auditRunStatusFailed
		slog.Error(
			"Запуск завершується через послідовні помилки heartbeat",
			"error",
			context.Cause(workCtx),
			"unprocessed_targets",
			unprocessedTargets,
			"saved_results",
			summary.Saved,
		)
		return exitFatal
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
	if summary.PersistenceFailures > 0 || missingResults > 0 {
		runCompletion.Status = auditRunStatusFailed
		slog.Error(
			"Запуск не може бути завершений через втрату результатів у persistence pipeline",
			"persistence_failures",
			summary.PersistenceFailures,
			"missing_results",
			missingResults,
			"saved_results",
			summary.Saved,
		)
		return exitFatal
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
	if targetSnapshot.Failed > 0 || streamSummary.Skipped > 0 || summary.Failed > 0 {
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

func closeResultsAfterProducers(
	workers *sync.WaitGroup,
	streamFinished <-chan struct{},
	results chan Result,
) {
	workers.Wait()
	<-streamFinished
	close(results)
}

func guardGracefulShutdown(
	lifecycleCtx context.Context,
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
		case <-lifecycleCtx.Done():
			slog.Warn(
				"Lifecycle запуску зупиняє планування нових URL",
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
