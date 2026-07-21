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

			_, err = tx.Exec(
				dbWriteCtx,
				`INSERT INTO audit_run_targets (run_id, target_id, request_url)
				 SELECT $1, id, url
				 FROM pages_to_scan
				 WHERE is_active = TRUE
				 ORDER BY id
				 ON CONFLICT (run_id, target_id) DO UPDATE
				 SET request_url = EXCLUDED.request_url`,
				cfg.RunID,
			)
			if err != nil {
				return err
			}

			if err := tx.QueryRow(
				dbWriteCtx,
				`SELECT COALESCE(MAX(target_id), 0), COUNT(*)
				 FROM audit_run_targets
				 WHERE run_id = $1`,
				cfg.RunID,
			).Scan(&snapshot.HighWatermark, &snapshot.Total); err != nil {
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
