-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_runs
    ADD COLUMN IF NOT EXISTS owner_generation BIGINT NOT NULL DEFAULT 1;

ALTER TABLE audit_runs
    DROP CONSTRAINT IF EXISTS audit_runs_owner_generation_positive;

ALTER TABLE audit_runs
    ADD CONSTRAINT audit_runs_owner_generation_positive
    CHECK (owner_generation > 0);

ALTER TABLE audit_run_targets
    ADD COLUMN IF NOT EXISTS claim_generation BIGINT;

UPDATE audit_run_targets AS target
SET claim_generation = run.owner_generation
FROM audit_runs AS run
WHERE run.id = target.run_id
  AND target.status = 'running'
  AND target.claim_generation IS NULL;

ALTER TABLE audit_run_targets
    DROP CONSTRAINT IF EXISTS audit_run_targets_claim_generation_positive;

ALTER TABLE audit_run_targets
    ADD CONSTRAINT audit_run_targets_claim_generation_positive
    CHECK (claim_generation IS NULL OR claim_generation > 0);

COMMIT;

-- +goose Down
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_run_targets
    DROP CONSTRAINT IF EXISTS audit_run_targets_claim_generation_positive;

ALTER TABLE audit_run_targets
    DROP COLUMN IF EXISTS claim_generation;

ALTER TABLE audit_runs
    DROP CONSTRAINT IF EXISTS audit_runs_owner_generation_positive;

ALTER TABLE audit_runs
    DROP COLUMN IF EXISTS owner_generation;

COMMIT;
