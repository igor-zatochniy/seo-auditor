package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

func claimTargetURLBatch(
	ctx context.Context,
	dbPool *pgxpool.Pool,
	cfg Config,
	limit int,
) ([]targetURLRecord, error) {
	dbFetchCtx, fetchCancel := context.WithTimeout(ctx, cfg.DBFetchTimeout)
	defer fetchCancel()

	var records []targetURLRecord
	err := retryDBMutation(
		dbFetchCtx,
		"claim_target_url_batch",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			rows, err := dbPool.Query(
				dbFetchCtx,
				`WITH claimable AS (
				     SELECT target.target_id
				     FROM audit_run_targets AS target
				     JOIN audit_runs AS run ON run.id = target.run_id
				     WHERE target.run_id = $1
				       AND run.status = $2
				       AND run.worker_instance_id = $3
				       AND (
				           target.status = $4
				           OR (
				               target.status = $5
				               AND target.lease_until < CURRENT_TIMESTAMP
				               AND target.claimed_by IS DISTINCT FROM $3
				           )
				       )
				     ORDER BY target.target_id
				     FOR UPDATE OF target SKIP LOCKED
				     LIMIT $6
				 )
				 UPDATE audit_run_targets AS target
				 SET status = $5,
				     attempts = target.attempts + 1,
				     claimed_by = $3,
				     claimed_at = CURRENT_TIMESTAMP,
				     lease_until = CURRENT_TIMESTAMP + ($7 * INTERVAL '1 millisecond'),
				     finished_at = NULL,
				     last_error = ''
				 FROM claimable
				 WHERE target.run_id = $1
				   AND target.target_id = claimable.target_id
				 RETURNING target.target_id, target.request_url`,
				cfg.RunID,
				auditRunStatusRunning,
				effectiveWorkerInstanceID(cfg),
				auditTargetStatusPending,
				auditTargetStatusRunning,
				limit,
				effectiveTargetLeaseDuration(cfg).Milliseconds(),
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
		return nil, fmt.Errorf("claim target URL batch: %w", err)
	}

	return records, nil
}

func streamTargetURLs(
	ctx context.Context,
	batchSize int,
	cfg Config,
	jobs chan<- AuditTarget,
	results chan<- Result,
	claimBatch targetURLBatchClaimer,
) urlStreamSummary {
	summary := urlStreamSummary{}
	if batchSize <= 0 {
		summary.Error = fmt.Errorf("target URL batch size must be positive")
		return summary
	}
	for {
		batch, err := claimBatch(ctx, batchSize)
		if err != nil {
			summary.Error = err
			return summary
		}
		if len(batch) == 0 {
			break
		}

		for _, record := range batch {
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
