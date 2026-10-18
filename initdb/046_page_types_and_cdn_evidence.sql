-- +goose Up
CREATE TABLE audit_page_type_settings (
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0),
    rules JSONB NOT NULL CHECK (jsonb_typeof(rules)='array' AND jsonb_array_length(rules)<=32 AND octet_length(rules::text)<=32768),
    PRIMARY KEY(run_id,origin)
);
CREATE TABLE audit_page_type_overrides (
    run_id UUID NOT NULL,
    target_id BIGINT NOT NULL,
    page_type VARCHAR(16) NOT NULL CHECK (page_type IN ('auto','product','category','article','service','other')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY(run_id,target_id),
    FOREIGN KEY(run_id,target_id) REFERENCES audit_results(run_id,target_id) ON DELETE CASCADE
);
ALTER TABLE audit_bot_imports DROP CONSTRAINT audit_bot_imports_format_check;
ALTER TABLE audit_bot_imports ADD CONSTRAINT audit_bot_imports_format_check
    CHECK (format IN ('jsonl','combined','cloudfront','cloudflare','vercel','fastly'));
ALTER TABLE audit_bot_events ADD COLUMN origin_status INT CHECK (origin_status BETWEEN 100 AND 599);
ALTER TABLE audit_bot_events ADD COLUMN cache_status VARCHAR(32);
ALTER TABLE audit_bot_events ADD COLUMN event_fingerprint BYTEA CHECK (event_fingerprint IS NULL OR octet_length(event_fingerprint)=32);

-- +goose Down
ALTER TABLE audit_bot_events DROP COLUMN event_fingerprint;
ALTER TABLE audit_bot_events DROP COLUMN cache_status;
ALTER TABLE audit_bot_events DROP COLUMN origin_status;
ALTER TABLE audit_bot_imports DROP CONSTRAINT audit_bot_imports_format_check;
ALTER TABLE audit_bot_imports ADD CONSTRAINT audit_bot_imports_format_check
    CHECK (format IN ('jsonl','combined')) NOT VALID;
DROP TABLE audit_page_type_overrides;
DROP TABLE audit_page_type_settings;
