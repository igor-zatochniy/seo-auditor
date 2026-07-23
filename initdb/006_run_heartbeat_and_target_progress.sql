-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_runs
    ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS worker_instance_id VARCHAR(128) NOT NULL DEFAULT 'legacy';

UPDATE audit_runs
SET heartbeat_at = COALESCE(heartbeat_at, finished_at, started_at, CURRENT_TIMESTAMP)
WHERE heartbeat_at IS NULL;

ALTER TABLE audit_runs
    ALTER COLUMN heartbeat_at SET NOT NULL;

ALTER TABLE audit_runs
    DROP CONSTRAINT IF EXISTS audit_runs_status_check;

ALTER TABLE audit_runs
    ADD CONSTRAINT audit_runs_status_check
    CHECK (status IN ('running', 'completed', 'completed_with_errors', 'failed', 'canceled', 'abandoned'));

ALTER TABLE audit_run_targets
    ADD COLUMN IF NOT EXISTS status VARCHAR(32) NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS claimed_by VARCHAR(128),
    ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS finished_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';

UPDATE audit_run_targets AS target
SET status = 'completed',
    finished_at = COALESCE(target.finished_at, result.created_at)
FROM audit_results AS result
WHERE target.run_id = result.run_id
  AND target.target_id = result.target_id
  AND target.status = 'pending'
  AND result.scan_status <> 'failed';

UPDATE audit_run_targets AS target
SET status = 'failed',
    finished_at = COALESCE(target.finished_at, result.created_at),
    last_error = COALESCE(NULLIF(result.error_message, ''), target.last_error)
FROM audit_results AS result
WHERE target.run_id = result.run_id
  AND target.target_id = result.target_id
  AND target.status = 'pending'
  AND result.scan_status = 'failed';

ALTER TABLE audit_run_targets
    DROP CONSTRAINT IF EXISTS audit_run_targets_status_check,
    DROP CONSTRAINT IF EXISTS audit_run_targets_attempts_check;

ALTER TABLE audit_run_targets
    ADD CONSTRAINT audit_run_targets_status_check
    CHECK (status IN ('pending', 'running', 'completed', 'failed', 'canceled', 'abandoned')),
    ADD CONSTRAINT audit_run_targets_attempts_check
    CHECK (attempts >= 0);

CREATE INDEX IF NOT EXISTS idx_audit_runs_heartbeat_status
    ON audit_runs(status, heartbeat_at);

CREATE INDEX IF NOT EXISTS idx_audit_run_targets_run_status
    ON audit_run_targets(run_id, status);

CREATE INDEX IF NOT EXISTS idx_audit_run_targets_claimed
    ON audit_run_targets(claimed_by, claimed_at)
    WHERE claimed_by IS NOT NULL;

COMMIT;
