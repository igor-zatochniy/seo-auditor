-- +goose Up
CREATE TABLE audit_bot_networks (
    bot VARCHAR(32) PRIMARY KEY,
    prefixes JSONB NOT NULL CHECK (jsonb_typeof(prefixes)='array' AND octet_length(prefixes::TEXT)<=262144),
    fetched_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE audit_bot_imports (
    id UUID PRIMARY KEY,
    domain VARCHAR(253) NOT NULL,
    digest BYTEA NOT NULL CHECK (octet_length(digest)=32),
    fingerprint_key_id VARCHAR(32) NOT NULL,
    format VARCHAR(16) NOT NULL CHECK (format IN ('jsonl','combined')),
    stats JSONB NOT NULL,
    range_evidence JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(domain,digest,fingerprint_key_id)
);
CREATE TABLE audit_bot_events (
    import_id UUID NOT NULL REFERENCES audit_bot_imports(id) ON DELETE CASCADE,
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 10000),
    safe_url TEXT NOT NULL CHECK (length(safe_url)<=2048),
    target_fingerprint BYTEA NOT NULL CHECK (octet_length(target_fingerprint)=32),
    bot VARCHAR(32) NOT NULL,
    verification VARCHAR(32) NOT NULL CHECK (verification IN ('unverified','matched_range','range_mismatch')),
    http_status INT NOT NULL CHECK (http_status BETWEEN 100 AND 599),
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(import_id,ordinal)
);
CREATE INDEX audit_bot_imports_history ON audit_bot_imports(domain,created_at DESC,id);

-- +goose Down
DROP TABLE audit_bot_events;
DROP TABLE audit_bot_imports;
DROP TABLE audit_bot_networks;
