package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func captureAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (targetURLSnapshot, error) {
	alreadyCaptured, err := auditRunTargetsCaptured(ctx, dbPool, cfg)
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("capture audit run targets: %w", err)
	}
	if alreadyCaptured {
		snapshot, err := summarizeAuditRunTargets(ctx, dbPool, cfg)
		if err != nil {
			return targetURLSnapshot{}, fmt.Errorf("capture audit run targets: %w", err)
		}
		return snapshot, nil
	}

	if err := deletePartialAuditRunTargets(ctx, dbPool, cfg); err != nil {
		return targetURLSnapshot{}, fmt.Errorf("capture audit run targets: %w", err)
	}

	snapshot, err := copyAuditRunTargets(ctx, dbPool, cfg)
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("capture audit run targets: %w", err)
	}
	return snapshot, nil
}

func auditRunTargetsCaptured(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (bool, error) {
	var captured bool
	err := withDBReadRetry(ctx, cfg, "read_audit_target_capture_state", func(queryCtx context.Context) error {
		return dbPool.QueryRow(
			queryCtx,
			`SELECT targets_captured_at IS NOT NULL
			 FROM audit_runs
			 WHERE id = $1
			   AND status = $2
			   AND worker_instance_id = $3`,
			cfg.RunID,
			auditRunStatusRunning,
			effectiveWorkerInstanceID(cfg),
		).Scan(&captured)
	})
	return captured, err
}

func deletePartialAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) error {
	for {
		var deleted int64
		err := withDBMutationRetry(ctx, cfg, "delete_partial_audit_run_targets", func(queryCtx context.Context) error {
			commandTag, err := dbPool.Exec(
				queryCtx,
				`WITH target_batch AS (
				     SELECT target.target_id
				     FROM audit_run_targets AS target
				     WHERE target.run_id = $1
				     ORDER BY target.target_id
				     LIMIT $2
				 )
				 DELETE FROM audit_run_targets AS target
				 USING target_batch
				 WHERE target.run_id = $1
				   AND target.target_id = target_batch.target_id
				   AND EXISTS (
				       SELECT 1
				       FROM audit_runs AS run
				       WHERE run.id = $1
				         AND run.status = $3
				         AND run.worker_instance_id = $4
				         AND run.targets_captured_at IS NULL
				   )`,
				cfg.RunID,
				effectiveURLBatchSize(cfg),
				auditRunStatusRunning,
				effectiveWorkerInstanceID(cfg),
			)
			if err != nil {
				return err
			}
			deleted = commandTag.RowsAffected()
			return nil
		})
		if err != nil {
			return fmt.Errorf("delete partial target batch: %w", err)
		}
		if deleted == 0 {
			return nil
		}
	}
}

func copyAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (targetURLSnapshot, error) {
	beginCtx, beginCancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(cfg))
	tx, err := dbPool.BeginTx(beginCtx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	beginCancel()
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("begin stable source snapshot: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), effectiveDBWriteTimeout(cfg))
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	snapshot := targetURLSnapshot{}
	lastID := int64(math.MinInt32)
	for {
		records, err := readActiveTargetBatch(ctx, tx, lastID, effectiveURLBatchSize(cfg), cfg)
		if err != nil {
			return targetURLSnapshot{}, err
		}
		if len(records) == 0 {
			break
		}
		if err := insertAuditRunTargetBatch(ctx, dbPool, cfg, records); err != nil {
			return targetURLSnapshot{}, err
		}

		lastID = records[len(records)-1].ID
		snapshot.HighWatermark = lastID
		snapshot.Total += int64(len(records))
	}

	commitCtx, commitCancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(cfg))
	err = tx.Commit(commitCtx)
	commitCancel()
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("close stable source snapshot: %w", err)
	}

	err = withDBMutationRetry(ctx, cfg, "finish_audit_target_capture", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`UPDATE audit_runs
			 SET targets_captured_at = CURRENT_TIMESTAMP,
			     total_urls = $4,
			     successful_urls = 0,
			     failed_urls = 0
			 WHERE id = $1
			   AND status = $2
			   AND worker_instance_id = $3
			   AND targets_captured_at IS NULL`,
			cfg.RunID,
			auditRunStatusRunning,
			effectiveWorkerInstanceID(cfg),
			snapshot.Total,
		)
		if err != nil {
			return err
		}
		if commandTag.RowsAffected() != 1 {
			return fmt.Errorf("running audit run %s is no longer available for target capture", cfg.RunID)
		}
		return nil
	})
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("finish target capture: %w", err)
	}

	return snapshot, nil
}

func readActiveTargetBatch(
	ctx context.Context,
	tx pgx.Tx,
	lastID int64,
	limit int,
	cfg Config,
) ([]targetURLRecord, error) {
	var records []targetURLRecord
	err := withDBReadTimeout(ctx, cfg, func(queryCtx context.Context) error {
		rows, err := tx.Query(
			queryCtx,
			`SELECT id, url
			 FROM pages_to_scan
			 WHERE is_active = TRUE
			   AND id > $1
			 ORDER BY id
			 LIMIT $2`,
			lastID,
			limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		batch := make([]targetURLRecord, 0, limit)
		for rows.Next() {
			var record targetURLRecord
			if err := rows.Scan(&record.ID, &record.URL); err != nil {
				return err
			}
			batch = append(batch, record)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		records = batch
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read active target batch: %w", err)
	}
	return records, nil
}

func insertAuditRunTargetBatch(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	cfg Config,
	records []targetURLRecord,
) error {
	targetIDs := make([]int64, len(records))
	requestURLs := make([]string, len(records))
	for index, record := range records {
		targetIDs[index] = record.ID
		requestURLs[index] = record.URL
	}

	err := withDBMutationRetry(ctx, cfg, "insert_audit_run_target_batch", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`INSERT INTO audit_run_targets (run_id, target_id, request_url)
			 SELECT $1, source.target_id, source.request_url
			 FROM unnest($2::BIGINT[], $3::TEXT[]) AS source(target_id, request_url)
			 WHERE EXISTS (
			     SELECT 1
			     FROM audit_runs AS run
			     WHERE run.id = $1
			       AND run.status = $4
			       AND run.worker_instance_id = $5
			       AND run.targets_captured_at IS NULL
			 )
			 ON CONFLICT (run_id, target_id) DO NOTHING`,
			cfg.RunID,
			targetIDs,
			requestURLs,
			auditRunStatusRunning,
			effectiveWorkerInstanceID(cfg),
		)
		if err != nil {
			return err
		}
		if commandTag.RowsAffected() != int64(len(records)) {
			return fmt.Errorf(
				"inserted %d of %d audit targets for run %s",
				commandTag.RowsAffected(),
				len(records),
				cfg.RunID,
			)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("insert target batch: %w", err)
	}
	return nil
}

func summarizeAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (targetURLSnapshot, error) {
	snapshot := targetURLSnapshot{}
	lastID := int64(math.MinInt64)

	for {
		type targetState struct {
			id     int64
			status string
		}
		var states []targetState
		err := withDBReadRetry(ctx, cfg, "summarize_audit_run_target_batch", func(queryCtx context.Context) error {
			rows, err := dbPool.Query(
				queryCtx,
				`SELECT target_id, status
				 FROM audit_run_targets
				 WHERE run_id = $1
				   AND target_id > $2
				 ORDER BY target_id
				 LIMIT $3`,
				cfg.RunID,
				lastID,
				effectiveURLBatchSize(cfg),
			)
			if err != nil {
				return err
			}
			defer rows.Close()

			batch := make([]targetState, 0, effectiveURLBatchSize(cfg))
			for rows.Next() {
				var state targetState
				if err := rows.Scan(&state.id, &state.status); err != nil {
					return err
				}
				batch = append(batch, state)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			states = batch
			return nil
		})
		if err != nil {
			return targetURLSnapshot{}, fmt.Errorf("summarize target batch: %w", err)
		}
		if len(states) == 0 {
			break
		}

		for _, state := range states {
			snapshot.Total++
			switch state.status {
			case auditTargetStatusCompleted:
				snapshot.Successful++
			case auditTargetStatusFailed:
				snapshot.Failed++
			}
		}
		lastID = states[len(states)-1].id
		snapshot.HighWatermark = lastID
	}

	err := withDBMutationRetry(ctx, cfg, "refresh_audit_run_target_counts", func(queryCtx context.Context) error {
		commandTag, err := dbPool.Exec(
			queryCtx,
			`UPDATE audit_runs
			 SET total_urls = $4,
			     successful_urls = $5,
			     failed_urls = $6
			 WHERE id = $1
			   AND status = $2
			   AND worker_instance_id = $3`,
			cfg.RunID,
			auditRunStatusRunning,
			effectiveWorkerInstanceID(cfg),
			snapshot.Total,
			snapshot.Successful,
			snapshot.Failed,
		)
		if err != nil {
			return err
		}
		if commandTag.RowsAffected() != 1 {
			return fmt.Errorf("running audit run %s does not exist", cfg.RunID)
		}
		return nil
	})
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("refresh target counts: %w", err)
	}

	return snapshot, nil
}

func withDBReadRetry(
	ctx context.Context,
	cfg Config,
	operation string,
	fn func(context.Context) error,
) error {
	operationCtx, cancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(cfg))
	defer cancel()
	return retryDBOperation(
		operationCtx,
		operation,
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			return fn(operationCtx)
		},
	)
}

func withDBReadTimeout(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	operationCtx, cancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(cfg))
	defer cancel()
	return fn(operationCtx)
}

func withDBMutationRetry(
	ctx context.Context,
	cfg Config,
	operation string,
	fn func(context.Context) error,
) error {
	operationCtx, cancel := context.WithTimeout(ctx, effectiveDBWriteTimeout(cfg))
	defer cancel()
	return retryDBMutation(
		operationCtx,
		operation,
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			return fn(operationCtx)
		},
	)
}

func effectiveURLBatchSize(cfg Config) int {
	if cfg.URLBatchSize > 0 {
		return cfg.URLBatchSize
	}
	return DefaultURLBatchSize
}

func effectiveDBFetchTimeout(cfg Config) time.Duration {
	if cfg.DBFetchTimeout > 0 {
		return cfg.DBFetchTimeout
	}
	return DefaultDBFetchTimeout
}
