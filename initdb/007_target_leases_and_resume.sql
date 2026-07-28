-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_runs
    ADD COLUMN IF NOT EXISTS targets_captured_at TIMESTAMP WITH TIME ZONE;

UPDATE audit_runs
SET targets_captured_at = COALESCE(targets_captured_at, started_at)
WHERE targets_captured_at IS NULL;

ALTER TABLE audit_run_targets
    ADD COLUMN IF NOT EXISTS lease_until TIMESTAMP WITH TIME ZONE;

UPDATE audit_run_targets
SET lease_until = NULL
WHERE status <> 'running';

CREATE INDEX IF NOT EXISTS idx_audit_run_targets_claimable
    ON audit_run_targets(run_id, status, lease_until, target_id);

COMMIT;

-- +goose Down
-- +goose NO TRANSACTION

BEGIN;

DROP INDEX IF EXISTS idx_audit_run_targets_claimable;

ALTER TABLE audit_run_targets
    DROP COLUMN IF EXISTS lease_until;

ALTER TABLE audit_runs
    DROP COLUMN IF EXISTS targets_captured_at;

COMMIT;
