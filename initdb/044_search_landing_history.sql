-- +goose Up
CREATE TABLE audit_search_imports (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash)=32),
    fingerprint_key_id VARCHAR(32) NOT NULL,
    row_count INT NOT NULL CHECK (row_count BETWEEN 1 AND 10000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(run_id, origin, payload_hash)
);
CREATE TABLE audit_search_rows (
    import_id UUID NOT NULL REFERENCES audit_search_imports(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    day DATE NOT NULL,
    query TEXT NOT NULL CHECK (char_length(query) BETWEEN 1 AND 300),
    query_key TEXT NOT NULL CHECK (octet_length(query_key) BETWEEN 1 AND 1200),
    safe_url TEXT NOT NULL CHECK (char_length(safe_url)<=2048),
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    country VARCHAR(3) NOT NULL,
    device VARCHAR(7) NOT NULL CHECK (device IN ('desktop','mobile','tablet')),
    impressions BIGINT NOT NULL CHECK (impressions BETWEEN 0 AND 1000000000000),
    clicks BIGINT NOT NULL CHECK (clicks BETWEEN 0 AND impressions),
    position DOUBLE PRECISION CHECK (position BETWEEN 1 AND 10000),
    PRIMARY KEY (run_id, origin, day, query_key, country, device, fingerprint)
);
CREATE INDEX audit_search_rows_import ON audit_search_rows(import_id);
CREATE INDEX audit_search_rows_history ON audit_search_rows(run_id, origin, query_key, country, device, day DESC);

-- +goose Down
DROP TABLE audit_search_rows;
DROP TABLE audit_search_imports;
