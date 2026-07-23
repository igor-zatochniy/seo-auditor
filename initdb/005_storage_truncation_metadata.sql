-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_results
    ADD COLUMN IF NOT EXISTS safe_url_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS safe_url_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS redirect_url_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS redirect_url_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS title_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS title_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS h1_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS h1_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS og_title_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS og_title_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS og_image_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS og_image_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS twitter_card_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS twitter_card_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS canonical_url_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS canonical_url_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS meta_robots_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS meta_robots_original_length INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS x_robots_tag_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS x_robots_tag_original_length INT NOT NULL DEFAULT 0;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'audit_results'::regclass
          AND conname = 'audit_results_truncation_lengths_check'
    ) THEN
        ALTER TABLE audit_results
            ADD CONSTRAINT audit_results_truncation_lengths_check
            CHECK (
                safe_url_original_length >= 0
                AND redirect_url_original_length >= 0
                AND title_original_length >= 0
                AND h1_original_length >= 0
                AND og_title_original_length >= 0
                AND og_image_original_length >= 0
                AND twitter_card_original_length >= 0
                AND canonical_url_original_length >= 0
                AND meta_robots_original_length >= 0
                AND x_robots_tag_original_length >= 0
            );
    END IF;
END
$$;
-- +goose StatementEnd

COMMIT;
