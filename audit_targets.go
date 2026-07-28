package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func captureAuditRunTargets(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) (targetURLSnapshot, error) {
	dbWriteCtx, cancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
	defer cancel()

	var snapshot targetURLSnapshot
	err := retryDBOperation(
		dbWriteCtx,
		"capture_audit_run_targets",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			tx, err := dbPool.BeginTx(dbWriteCtx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			defer func() {
				_ = tx.Rollback(dbWriteCtx)
			}()

			var alreadyCaptured bool
			if err := tx.QueryRow(
				dbWriteCtx,
				`SELECT targets_captured_at IS NOT NULL
				 FROM audit_runs
				 WHERE id = $1
				   AND status = $2
				   AND worker_instance_id = $3
				 FOR UPDATE`,
				cfg.RunID,
				auditRunStatusRunning,
				effectiveWorkerInstanceID(cfg),
			).Scan(&alreadyCaptured); err != nil {
				return err
			}

			if !alreadyCaptured {
				if _, err := tx.Exec(
					dbWriteCtx,
					`INSERT INTO audit_run_targets (run_id, target_id, request_url)
					 SELECT $1, id, url
					 FROM pages_to_scan
					 WHERE is_active = TRUE
					 ORDER BY id
					 ON CONFLICT (run_id, target_id) DO NOTHING`,
					cfg.RunID,
				); err != nil {
					return err
				}

				if _, err := tx.Exec(
					dbWriteCtx,
					`UPDATE audit_runs
					 SET targets_captured_at = CURRENT_TIMESTAMP
					 WHERE id = $1`,
					cfg.RunID,
				); err != nil {
					return err
				}
			}

			if err := tx.QueryRow(
				dbWriteCtx,
				`SELECT
				     COALESCE(MAX(target_id), 0),
				     COUNT(*),
				     COUNT(*) FILTER (WHERE status = $2),
				     COUNT(*) FILTER (WHERE status = $3)
				 FROM audit_run_targets
				 WHERE run_id = $1`,
				cfg.RunID,
				auditTargetStatusCompleted,
				auditTargetStatusFailed,
			).Scan(
				&snapshot.HighWatermark,
				&snapshot.Total,
				&snapshot.Successful,
				&snapshot.Failed,
			); err != nil {
				return err
			}

			if _, err := tx.Exec(
				dbWriteCtx,
				`UPDATE audit_runs
				 SET total_urls = $2,
				     successful_urls = $3,
				     failed_urls = $4
				 WHERE id = $1`,
				cfg.RunID,
				snapshot.Total,
				snapshot.Successful,
				snapshot.Failed,
			); err != nil {
				return err
			}

			if err := tx.Commit(dbWriteCtx); err != nil {
				return err
			}
			return nil
		},
	)
	if err != nil {
		return targetURLSnapshot{}, fmt.Errorf("capture audit run targets: %w", err)
	}

	return snapshot, nil
}
