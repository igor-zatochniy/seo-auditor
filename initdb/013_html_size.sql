-- +goose Up
ALTER TABLE audit_results
    ADD COLUMN html_raw_bytes BIGINT CHECK (html_raw_bytes >= 0),
    ADD COLUMN html_size_complete BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN googlebot_2mb_status TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE audit_results
    DROP COLUMN googlebot_2mb_status,
    DROP COLUMN html_size_complete,
    DROP COLUMN html_raw_bytes;
