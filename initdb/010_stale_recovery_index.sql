-- +goose Up
-- +goose NO TRANSACTION

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_run_targets_incomplete
    ON audit_run_targets(run_id, target_id)
    WHERE status IN ('pending', 'running');

-- +goose Down
-- +goose NO TRANSACTION

DROP INDEX CONCURRENTLY IF EXISTS idx_audit_run_targets_incomplete;
