package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	auditRunStatusRunning             = "running"
	auditRunStatusCompleted           = "completed"
	auditRunStatusCompletedWithErrors = "completed_with_errors"
	auditRunStatusFailed              = "failed"
	auditRunStatusCanceled            = "canceled"
	auditRunStatusAbandoned           = "abandoned"

	auditTargetStatusPending   = "pending"
	auditTargetStatusRunning   = "running"
	auditTargetStatusCompleted = "completed"
	auditTargetStatusFailed    = "failed"
	auditTargetStatusCanceled  = "canceled"
	auditTargetStatusAbandoned = "abandoned"
)

type auditRunCompletion struct {
	Status         string
	TotalURLs      int64
	SuccessfulURLs int
	FailedURLs     int
}

func createAuditRun(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) error {
	var created bool
	err := withDBMutationRetry(ctx, cfg, "create_audit_run", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`INSERT INTO audit_runs (id, started_at, heartbeat_at, worker_instance_id, status)
			 VALUES ($1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, $2, $3)
			 ON CONFLICT (id) DO NOTHING`,
			cfg.RunID,
			effectiveWorkerInstanceID(cfg),
			auditRunStatusRunning,
		)
		if err != nil {
			return err
		}
		created = commandTag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return fmt.Errorf("create audit run %s: %w", cfg.RunID, err)
	}
	if created {
		return nil
	}

	var currentStatus string
	if err := withDBReadRetry(ctx, cfg, "read_resumable_audit_run", func(queryCtx context.Context) error {
		return dbPool.QueryRow(
			queryCtx,
			`SELECT status
			 FROM audit_runs
			 WHERE id = $1`,
			cfg.RunID,
		).Scan(&currentStatus)
	}); err != nil {
		return fmt.Errorf("read audit run %s for resume: %w", cfg.RunID, err)
	}
	if currentStatus != auditRunStatusAbandoned && currentStatus != auditRunStatusFailed {
		return fmt.Errorf("audit run %s already exists and is not resumable", cfg.RunID)
	}

	if err := resetResumableAuditRunTargets(ctx, dbPool, cfg); err != nil {
		return fmt.Errorf("reset audit run %s targets for resume: %w", cfg.RunID, err)
	}
	err = withDBMutationRetry(ctx, cfg, "resume_audit_run", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`UPDATE audit_runs
			 SET finished_at = NULL,
			     heartbeat_at = CURRENT_TIMESTAMP,
			     worker_instance_id = $2,
			     status = $3
			 WHERE id = $1
			   AND status IN ($4, $5)`,
			cfg.RunID,
			effectiveWorkerInstanceID(cfg),
			auditRunStatusRunning,
			auditRunStatusAbandoned,
			auditRunStatusFailed,
		)
		if err != nil {
			return err
		}
		if commandTag.RowsAffected() != 1 {
			return fmt.Errorf("audit run %s was resumed by another worker", cfg.RunID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("resume audit run %s: %w", cfg.RunID, err)
	}
	return nil
}

func resetResumableAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) error {
	for {
		var reset int64
		err := withDBMutationRetry(ctx, cfg, "reset_resumable_audit_target_batch", func(queryCtx context.Context) error {
			commandTag, err := dbPool.Exec(
				queryCtx,
				`WITH target_batch AS (
				     SELECT target.target_id
				     FROM audit_run_targets AS target
				     WHERE target.run_id = $1
				       AND (
				           target.status = $3
				           OR (
				               target.status = $4
				               AND target.request_url <> ''
				               AND target.request_url_cleared_at IS NULL
				           )
				       )
				     ORDER BY target.target_id
				     LIMIT $2
				     FOR UPDATE SKIP LOCKED
				 )
				 UPDATE audit_run_targets AS target
				 SET status = $5,
				     claimed_by = NULL,
				     claimed_at = NULL,
				     lease_until = NULL,
				     finished_at = NULL,
				     last_error = ''
				 FROM target_batch
				 WHERE target.run_id = $1
				   AND target.target_id = target_batch.target_id
				   AND EXISTS (
				       SELECT 1
				       FROM audit_runs AS run
				       WHERE run.id = $1
				         AND run.status IN ($6, $7)
				   )`,
				cfg.RunID,
				effectiveURLBatchSize(cfg),
				auditTargetStatusAbandoned,
				auditTargetStatusCanceled,
				auditTargetStatusPending,
				auditRunStatusAbandoned,
				auditRunStatusFailed,
			)
			if err != nil {
				return err
			}
			reset = commandTag.RowsAffected()
			return nil
		})
		if err != nil {
			return err
		}
		if reset == 0 {
			return nil
		}
	}
}

func completeAuditRun(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	completion auditRunCompletion,
	cfg Config,
) error {
	if err := validateAuditRunCompletion(completion); err != nil {
		return err
	}

	if err := markIncompleteTargetsForRunCompletion(ctx, dbPool, runID, completion.Status, cfg); err != nil {
		return fmt.Errorf("prepare audit run %s completion: %w", runID, err)
	}
	if err := finalizeAuditRun(ctx, dbPool, runID, completion, cfg); err != nil {
		return fmt.Errorf("complete audit run %s: %w", runID, err)
	}
	if _, err := clearAuditRunTargetURLs(ctx, dbPool, runID, cfg); err != nil {
		return fmt.Errorf("clear retained URLs for audit run %s: %w", runID, err)
	}
	return nil
}

func finalizeAuditRun(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	completion auditRunCompletion,
	cfg Config,
) error {
	return withDBMutationRetry(ctx, cfg, "complete_audit_run", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`UPDATE audit_runs
			 SET finished_at = CURRENT_TIMESTAMP,
			     heartbeat_at = CURRENT_TIMESTAMP,
			     status = $2,
			     total_urls = $3,
			     successful_urls = $4,
			     failed_urls = $5
			 WHERE id = $1
			   AND status IN ($6, $2)
			   AND worker_instance_id = $7`,
			runID,
			completion.Status,
			completion.TotalURLs,
			completion.SuccessfulURLs,
			completion.FailedURLs,
			auditRunStatusRunning,
			effectiveWorkerInstanceID(cfg),
		)
		if err != nil {
			return err
		}
		if commandTag.RowsAffected() != 1 {
			return fmt.Errorf("audit run %s does not exist or has a conflicting terminal status", runID)
		}
		return nil
	})
}

func clearAuditRunTargetURLs(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	cfg Config,
) (int64, error) {
	var totalCleared int64
	for {
		var cleared int64
		err := withDBMutationRetry(ctx, cfg, "clear_audit_run_target_url_batch", func(queryCtx context.Context) error {
			commandTag, err := dbPool.Exec(
				queryCtx,
				`WITH target_batch AS (
				     SELECT target.target_id
				     FROM audit_run_targets AS target
				     JOIN audit_runs AS run ON run.id = target.run_id
				     WHERE target.run_id = $1
				       AND run.status IN ($3, $4, $5, $6)
				       AND target.request_url <> ''
				       AND target.status IN ($7, $8, $9)
				     ORDER BY target.target_id
				     LIMIT $2
				     FOR UPDATE OF target SKIP LOCKED
				 )
				 UPDATE audit_run_targets AS target
				 SET request_url = '',
				     request_url_cleared_at = CURRENT_TIMESTAMP
				 FROM target_batch
				 WHERE target.run_id = $1
				   AND target.target_id = target_batch.target_id`,
				runID,
				effectiveURLBatchSize(cfg),
				auditRunStatusCompleted,
				auditRunStatusCompletedWithErrors,
				auditRunStatusFailed,
				auditRunStatusCanceled,
				auditTargetStatusCompleted,
				auditTargetStatusFailed,
				auditTargetStatusCanceled,
			)
			if err != nil {
				return err
			}
			cleared = commandTag.RowsAffected()
			return nil
		})
		if err != nil {
			return totalCleared, err
		}
		totalCleared += cleared
		if cleared == 0 {
			return totalCleared, nil
		}
	}
}

func clearRetainedTerminalAuditRunTargetURLs(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	cfg Config,
) (int64, error) {
	var totalCleared int64
	for {
		var cleared int64
		err := withDBMutationRetry(ctx, cfg, "clear_retained_terminal_target_url_batch", func(queryCtx context.Context) error {
			commandTag, err := dbPool.Exec(
				queryCtx,
				`WITH target_batch AS (
				     SELECT target.run_id, target.target_id
				     FROM audit_run_targets AS target
				     JOIN audit_runs AS run ON run.id = target.run_id
				     WHERE run.status IN ($2, $3, $4, $5)
				       AND target.request_url <> ''
				       AND target.status IN ($6, $7, $8)
				     ORDER BY target.run_id, target.target_id
				     LIMIT $1
				     FOR UPDATE OF target SKIP LOCKED
				 )
				 UPDATE audit_run_targets AS target
				 SET request_url = '',
				     request_url_cleared_at = CURRENT_TIMESTAMP
				 FROM target_batch
				 WHERE target.run_id = target_batch.run_id
				   AND target.target_id = target_batch.target_id`,
				effectiveURLBatchSize(cfg),
				auditRunStatusCompleted,
				auditRunStatusCompletedWithErrors,
				auditRunStatusFailed,
				auditRunStatusCanceled,
				auditTargetStatusCompleted,
				auditTargetStatusFailed,
				auditTargetStatusCanceled,
			)
			if err != nil {
				return err
			}
			cleared = commandTag.RowsAffected()
			return nil
		})
		if err != nil {
			return totalCleared, err
		}
		totalCleared += cleared
		if cleared == 0 {
			return totalCleared, nil
		}
	}
}

func abandonStaleAuditRuns(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (int64, error) {
	cutoff := time.Now().Add(-effectiveStaleRunThreshold(cfg))
	var abandonedRuns int64
	var abandonedTargets int64
	err := retryDBOperation(
		ctx,
		"abandon_stale_audit_runs",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			return dbPool.QueryRow(
				ctx,
				`WITH abandoned_runs AS (
				     UPDATE audit_runs
				     SET status = $2,
				         finished_at = CURRENT_TIMESTAMP
				     WHERE status = $3
				       AND heartbeat_at < $1
				     RETURNING id
				 ),
				 abandoned_targets AS (
				 UPDATE audit_run_targets AS target
				     SET status = $4,
				         finished_at = COALESCE(target.finished_at, CURRENT_TIMESTAMP),
				         lease_until = NULL,
				         last_error = $5
				     FROM abandoned_runs
				     WHERE target.run_id = abandoned_runs.id
				       AND target.status NOT IN ($6, $7, $8, $4)
				     RETURNING 1
				 )
				 SELECT
				     (SELECT COUNT(*) FROM abandoned_runs),
				     (SELECT COUNT(*) FROM abandoned_targets)`,
				cutoff,
				auditRunStatusAbandoned,
				auditRunStatusRunning,
				auditTargetStatusAbandoned,
				"Audit run heartbeat expired before a clean shutdown.",
				auditTargetStatusCompleted,
				auditTargetStatusFailed,
				auditTargetStatusCanceled,
			).Scan(&abandonedRuns, &abandonedTargets)
		},
	)
	if err != nil {
		return 0, fmt.Errorf("abandon stale audit runs: %w", err)
	}
	return abandonedRuns, nil
}

func startAuditRunHeartbeat(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(effectiveAuditRunHeartbeat(cfg))
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(ctx, effectiveDBWriteTimeout(cfg))
				err := updateAuditRunHeartbeat(heartbeatCtx, dbPool, cfg)
				cancel()
				if err != nil {
					slog.Warn("Не вдалося оновити heartbeat запуску аудиту", "error", err)
				}
			}
		}
	}()
	return done
}

func updateAuditRunHeartbeat(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) error {
	err := retryDBOperation(
		ctx,
		"update_audit_run_heartbeat",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			tx, err := dbPool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			defer func() {
				_ = tx.Rollback(ctx)
			}()

			commandTag, err := tx.Exec(
				ctx,
				`UPDATE audit_runs
				 SET heartbeat_at = CURRENT_TIMESTAMP
				 WHERE id = $1
				   AND status = $3
				   AND worker_instance_id = $2`,
				cfg.RunID,
				effectiveWorkerInstanceID(cfg),
				auditRunStatusRunning,
			)
			if err != nil {
				return err
			}
			if commandTag.RowsAffected() != 1 {
				return fmt.Errorf("running audit run %s does not exist", cfg.RunID)
			}

			if _, err := tx.Exec(
				ctx,
				`UPDATE audit_run_targets
				 SET lease_until = CURRENT_TIMESTAMP + ($3 * INTERVAL '1 millisecond')
				 WHERE run_id = $1
				   AND status = $4
				   AND claimed_by = $2`,
				cfg.RunID,
				effectiveWorkerInstanceID(cfg),
				effectiveTargetLeaseDuration(cfg).Milliseconds(),
				auditTargetStatusRunning,
			); err != nil {
				return err
			}

			return tx.Commit(ctx)
		},
	)
	if err != nil {
		return fmt.Errorf("update audit run heartbeat %s: %w", cfg.RunID, err)
	}
	return nil
}

func markAuditRunTargetStarted(ctx context.Context, dbPool *pgxpool.Pool, target AuditTarget, cfg Config) error {
	if dbPool == nil {
		return nil
	}
	dbWriteCtx, cancel := context.WithTimeout(ctx, effectiveDBWriteTimeout(cfg))
	defer cancel()

	err := retryDBOperation(
		dbWriteCtx,
		"mark_audit_run_target_started",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			commandTag, err := dbPool.Exec(
				dbWriteCtx,
				`UPDATE audit_run_targets
				 SET claimed_at = COALESCE(claimed_at, CURRENT_TIMESTAMP),
				     lease_until = CURRENT_TIMESTAMP + ($5 * INTERVAL '1 millisecond'),
				     finished_at = NULL,
				     last_error = ''
				 WHERE run_id = $1
				   AND target_id = $2
				   AND status = $3
				   AND claimed_by = $4
				   AND EXISTS (
				       SELECT 1
				       FROM audit_runs
				       WHERE id = $1
				         AND status = $6
				         AND worker_instance_id = $4
				   )`,
				cfg.RunID,
				target.TargetID,
				auditTargetStatusRunning,
				effectiveWorkerInstanceID(cfg),
				effectiveTargetLeaseDuration(cfg).Milliseconds(),
				auditRunStatusRunning,
			)
			if err != nil {
				return err
			}
			if commandTag.RowsAffected() != 1 {
				return fmt.Errorf("target %d cannot be marked running for audit run %s", target.TargetID, cfg.RunID)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("mark audit target %d started: %w", target.TargetID, err)
	}
	return nil
}

func markAuditRunTargetFinished(
	ctx context.Context,
	tx pgx.Tx,
	runID string,
	targetID int64,
	workerInstanceID string,
	status string,
	lastError string,
) error {
	commandTag, err := tx.Exec(
		ctx,
		`UPDATE audit_run_targets
		 SET status = $3,
		     finished_at = CURRENT_TIMESTAMP,
		     lease_until = NULL,
		     last_error = $4
		 WHERE run_id = $1
		   AND target_id = $2
		   AND status = $5
		   AND claimed_by = $6
		   AND EXISTS (
		       SELECT 1
		       FROM audit_runs
		       WHERE id = $1
		         AND status = $7
		         AND worker_instance_id = $6
		   )`,
		runID,
		targetID,
		status,
		lastError,
		auditTargetStatusRunning,
		workerInstanceID,
		auditRunStatusRunning,
	)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() != 1 {
		return fmt.Errorf("target %d does not exist for audit run %s", targetID, runID)
	}
	return nil
}

func markIncompleteTargetsForRunCompletion(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	runID string,
	runStatus string,
	cfg Config,
) error {
	switch runStatus {
	case auditRunStatusCanceled:
		for {
			var updated int64
			err := withDBMutationRetry(ctx, cfg, "cancel_incomplete_audit_target_batch", func(queryCtx context.Context) error {
				commandTag, err := dbPool.Exec(
					queryCtx,
					`WITH target_batch AS (
					     SELECT target.target_id
					     FROM audit_run_targets AS target
					     WHERE target.run_id = $1
					       AND target.status IN ($3, $4, $5)
					     ORDER BY target.target_id
					     LIMIT $2
					     FOR UPDATE SKIP LOCKED
					 )
					 UPDATE audit_run_targets AS target
					 SET status = $6,
					     finished_at = CURRENT_TIMESTAMP,
					     lease_until = NULL,
					     last_error = CASE WHEN target.last_error = '' THEN $7 ELSE target.last_error END
					 FROM target_batch
					 WHERE target.run_id = $1
					   AND target.target_id = target_batch.target_id
					   AND EXISTS (
					       SELECT 1
					       FROM audit_runs AS run
					       WHERE run.id = $1
					         AND run.status = $8
					         AND run.worker_instance_id = $9
					   )`,
					runID,
					effectiveURLBatchSize(cfg),
					auditTargetStatusPending,
					auditTargetStatusRunning,
					auditTargetStatusAbandoned,
					auditTargetStatusCanceled,
					"Audit run was canceled before all targets finished.",
					auditRunStatusRunning,
					effectiveWorkerInstanceID(cfg),
				)
				if err != nil {
					return err
				}
				updated = commandTag.RowsAffected()
				return nil
			})
			if err != nil {
				return err
			}
			if updated == 0 {
				return nil
			}
		}
	case auditRunStatusFailed:
		for {
			var updated int64
			err := withDBMutationRetry(ctx, cfg, "release_incomplete_audit_target_batch", func(queryCtx context.Context) error {
				commandTag, err := dbPool.Exec(
					queryCtx,
					`WITH target_batch AS (
					     SELECT target.target_id
					     FROM audit_run_targets AS target
					     WHERE target.run_id = $1
					       AND target.status IN ($3, $4)
					     ORDER BY target.target_id
					     LIMIT $2
					     FOR UPDATE SKIP LOCKED
					 )
					 UPDATE audit_run_targets AS target
					 SET status = $5,
					     claimed_by = NULL,
					     claimed_at = NULL,
					     lease_until = NULL,
					     finished_at = NULL,
					     last_error = CASE WHEN target.last_error = '' THEN $6 ELSE target.last_error END
					 FROM target_batch
					 WHERE target.run_id = $1
					   AND target.target_id = target_batch.target_id
					   AND EXISTS (
					       SELECT 1
					       FROM audit_runs AS run
					       WHERE run.id = $1
					         AND run.status = $7
					         AND run.worker_instance_id = $8
					   )`,
					runID,
					effectiveURLBatchSize(cfg),
					auditTargetStatusRunning,
					auditTargetStatusAbandoned,
					auditTargetStatusPending,
					"Audit run failed before all targets finished; target is available for resume.",
					auditRunStatusRunning,
					effectiveWorkerInstanceID(cfg),
				)
				if err != nil {
					return err
				}
				updated = commandTag.RowsAffected()
				return nil
			})
			if err != nil {
				return err
			}
			if updated == 0 {
				return nil
			}
		}
	case auditRunStatusCompleted, auditRunStatusCompletedWithErrors:
		var incompleteTargetExists bool
		if err := withDBReadRetry(ctx, cfg, "check_incomplete_audit_targets", func(queryCtx context.Context) error {
			return dbPool.QueryRow(
				queryCtx,
				`SELECT EXISTS (
				     SELECT 1
				     FROM audit_run_targets
				     WHERE run_id = $1
				       AND status IN ($2, $3, $4, $5)
				 )`,
				runID,
				auditTargetStatusPending,
				auditTargetStatusRunning,
				auditTargetStatusCanceled,
				auditTargetStatusAbandoned,
			).Scan(&incompleteTargetExists)
		}); err != nil {
			return err
		}
		if incompleteTargetExists {
			return fmt.Errorf("audit run %s cannot complete with non-terminal targets", runID)
		}
		return nil
	default:
		return fmt.Errorf("unsupported audit run completion status %q", runStatus)
	}
}

func finalAuditTargetStatus(data SEOData, resultFailed bool) string {
	if resultFailed || data.ScanStatus == scanStatusFailed {
		return auditTargetStatusFailed
	}
	return auditTargetStatusCompleted
}

func effectiveWorkerInstanceID(cfg Config) string {
	if cfg.WorkerInstanceID != "" {
		return cfg.WorkerInstanceID
	}
	return "local-worker"
}

func effectiveAuditRunHeartbeat(cfg Config) time.Duration {
	if cfg.AuditRunHeartbeatInterval > 0 {
		return cfg.AuditRunHeartbeatInterval
	}
	return DefaultAuditRunHeartbeatInterval
}

func effectiveStaleRunThreshold(cfg Config) time.Duration {
	if cfg.StaleRunThreshold > 0 {
		return cfg.StaleRunThreshold
	}
	return DefaultStaleRunThreshold
}

func effectiveTargetLeaseDuration(cfg Config) time.Duration {
	if cfg.TargetLeaseDuration > 0 {
		return cfg.TargetLeaseDuration
	}
	return DefaultTargetLeaseDuration
}

func effectiveDBWriteTimeout(cfg Config) time.Duration {
	if cfg.DBWriteTimeout > 0 {
		return cfg.DBWriteTimeout
	}
	return DefaultDBWriteTimeout
}

func validateAuditRunCompletion(completion auditRunCompletion) error {
	switch completion.Status {
	case auditRunStatusCompleted, auditRunStatusCompletedWithErrors, auditRunStatusFailed, auditRunStatusCanceled:
	default:
		return fmt.Errorf("unsupported audit run status %q", completion.Status)
	}
	if completion.TotalURLs < 0 || completion.SuccessfulURLs < 0 || completion.FailedURLs < 0 {
		return fmt.Errorf("audit run counters must not be negative")
	}
	if int64(completion.SuccessfulURLs+completion.FailedURLs) > completion.TotalURLs {
		return fmt.Errorf("processed audit run counters exceed total URLs")
	}
	return nil
}
