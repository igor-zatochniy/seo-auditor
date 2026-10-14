-- +goose Up
CREATE TABLE audit_crawl_inventory_state (
    run_id UUID PRIMARY KEY REFERENCES audit_site_crawls(run_id) ON DELETE CASCADE,
    url_count INT NOT NULL DEFAULT 0 CHECK (url_count BETWEEN 0 AND 50000),
    limited BOOLEAN NOT NULL DEFAULT FALSE,
    captured BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE audit_crawl_inventory (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES audit_site_crawls(run_id) ON DELETE CASCADE,
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    safe_url TEXT NOT NULL CHECK (length(safe_url)<=2048),
    sources INT NOT NULL CHECK (sources BETWEEN 0 AND 15),
    deferred INT NOT NULL DEFAULT 0 CHECK (deferred BETWEEN 0 AND 7),
    important BOOLEAN NOT NULL DEFAULT FALSE,
    url_shape JSONB CHECK (url_shape IS NULL OR octet_length(url_shape::TEXT)<=4096),
    UNIQUE(run_id,fingerprint)
);
CREATE INDEX audit_crawl_inventory_page ON audit_crawl_inventory(run_id,id);
CREATE INDEX audit_crawl_inventory_family ON audit_crawl_inventory(run_id,(url_shape->>'family'));
CREATE TABLE audit_crawl_source_imports (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES audit_site_crawls(run_id) ON DELETE CASCADE,
    source VARCHAR(16) NOT NULL CHECK (source IN ('gsc','bots')),
    digest BYTEA NOT NULL CHECK (octet_length(digest)=32),
    bot_import_id UUID REFERENCES audit_bot_imports(id) ON DELETE RESTRICT,
    row_count INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(run_id,source,digest)
);
CREATE TABLE audit_crawl_bot_evidence (
    source_id UUID NOT NULL REFERENCES audit_crawl_source_imports(id) ON DELETE CASCADE,
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    visits BIGINT NOT NULL,
    matched_range_visits BIGINT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(source_id,fingerprint)
);
CREATE INDEX audit_crawl_bot_fingerprint ON audit_crawl_bot_evidence(fingerprint,source_id);
ALTER TABLE audit_bot_imports ADD COLUMN key_check BYTEA CHECK (key_check IS NULL OR octet_length(key_check)=32);
ALTER TABLE audit_bot_events ADD COLUMN url_shape JSONB CHECK (url_shape IS NULL OR octet_length(url_shape::TEXT)<=4096);
ALTER TABLE audit_bot_events ADD COLUMN coverage_fingerprint BYTEA CHECK (coverage_fingerprint IS NULL OR octet_length(coverage_fingerprint)=32);
CREATE INDEX audit_bot_coverage_identity ON audit_bot_events(import_id,coverage_fingerprint) WHERE coverage_fingerprint IS NOT NULL;

-- +goose Down
DROP INDEX audit_bot_coverage_identity;
ALTER TABLE audit_bot_events DROP COLUMN coverage_fingerprint;
ALTER TABLE audit_bot_events DROP COLUMN url_shape;
ALTER TABLE audit_bot_imports DROP COLUMN key_check;
DROP TABLE audit_crawl_bot_evidence;
DROP TABLE audit_crawl_source_imports;
DROP TABLE audit_crawl_inventory;
DROP TABLE audit_crawl_inventory_state;
