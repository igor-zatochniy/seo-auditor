-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

-- Зв'язуємо результат із конкретною строкою snapshot, не використовуючи
-- замаскований URL або fingerprint як первинний ідентифікатор.
CREATE TABLE IF NOT EXISTS audit_run_targets (
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    target_id BIGINT NOT NULL,
    request_url TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, target_id)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'audit_results'
          AND column_name = 'url'
    ) AND NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'audit_results'
          AND column_name = 'safe_url'
    ) THEN
        ALTER TABLE audit_results RENAME COLUMN url TO safe_url;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE audit_results
    ADD COLUMN IF NOT EXISTS target_fingerprint BYTEA;

UPDATE audit_results
SET target_fingerprint = decode(md5(run_id::TEXT || ':' || safe_url), 'hex')
WHERE target_fingerprint IS NULL;

ALTER TABLE audit_results
    ALTER COLUMN target_fingerprint SET NOT NULL;

ALTER TABLE audit_results
    ADD COLUMN IF NOT EXISTS target_id BIGINT;

INSERT INTO audit_run_targets (run_id, target_id, request_url)
SELECT result.run_id, -(result.id::BIGINT), result.safe_url
FROM audit_results AS result
WHERE result.target_id IS NULL
ON CONFLICT (run_id, target_id) DO NOTHING;

UPDATE audit_results
SET target_id = -(id::BIGINT)
WHERE target_id IS NULL;

ALTER TABLE audit_results
    ALTER COLUMN target_id SET NOT NULL;

ALTER TABLE audit_results
    DROP CONSTRAINT IF EXISTS audit_results_run_url_key;

ALTER TABLE audit_results
    DROP CONSTRAINT IF EXISTS audit_results_run_target_fingerprint_key;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'audit_results'::regclass
          AND conname = 'audit_results_run_target_id_key'
    ) THEN
        ALTER TABLE audit_results
            ADD CONSTRAINT audit_results_run_target_id_key UNIQUE (run_id, target_id);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'audit_results'::regclass
          AND conname = 'audit_results_run_target_id_fkey'
    ) THEN
        ALTER TABLE audit_results
            ADD CONSTRAINT audit_results_run_target_id_fkey
            FOREIGN KEY (run_id, target_id)
            REFERENCES audit_run_targets(run_id, target_id)
            ON DELETE CASCADE;
    END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS idx_audit_results_run_fingerprint
    ON audit_results(run_id, target_fingerprint);

COMMIT;
