-- +goose Up

ALTER TABLE audit_run_targets
    ADD COLUMN IF NOT EXISTS started_at TIMESTAMP WITH TIME ZONE;

-- Existing attempts were counted at claim time. Preserve their historical
-- meaning while providing the closest available start timestamp.
UPDATE audit_run_targets
SET started_at = COALESCE(claimed_at, finished_at, created_at)
WHERE attempts > 0
  AND started_at IS NULL;

-- +goose Down

ALTER TABLE audit_run_targets
    DROP COLUMN IF EXISTS started_at;
