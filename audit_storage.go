package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	auditRunStatusRunning             = "running"
	auditRunStatusCompleted           = "completed"
	auditRunStatusCompletedWithErrors = "completed_with_errors"
	auditRunStatusFailed              = "failed"
	auditRunStatusCanceled            = "canceled"
)

type auditRunCompletion struct {
	Status         string
	TotalURLs      int64
	SuccessfulURLs int
	FailedURLs     int
}

func createAuditRun(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) error {
	err := retryDBOperation(
		ctx,
		"create_audit_run",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			commandTag, err := dbPool.Exec(
				ctx,
				`INSERT INTO audit_runs (id, started_at, status)
				 VALUES ($1, CURRENT_TIMESTAMP, $2)`,
				cfg.RunID,
				auditRunStatusRunning,
			)
			if err != nil {
				return err
			}
			if commandTag.RowsAffected() != 1 {
				return fmt.Errorf("expected one inserted audit run, got %d", commandTag.RowsAffected())
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("create audit run %s: %w", cfg.RunID, err)
	}
	return nil
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

	err := retryDBOperation(
		ctx,
		"complete_audit_run",
		retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
		func() error {
			commandTag, err := dbPool.Exec(
				ctx,
				`UPDATE audit_runs
				 SET finished_at = CURRENT_TIMESTAMP,
				     status = $2,
				     total_urls = $3,
				     successful_urls = $4,
				     failed_urls = $5
				 WHERE id = $1`,
				runID,
				completion.Status,
				completion.TotalURLs,
				completion.SuccessfulURLs,
				completion.FailedURLs,
			)
			if err != nil {
				return err
			}
			if commandTag.RowsAffected() != 1 {
				return fmt.Errorf("audit run %s does not exist", runID)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("complete audit run %s: %w", runID, err)
	}
	return nil
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
