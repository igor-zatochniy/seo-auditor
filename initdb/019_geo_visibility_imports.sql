-- +goose Up
CREATE TABLE geo_visibility_imports (
    id UUID PRIMARY KEY,
    report_id UUID NOT NULL REFERENCES geo_reports(id) ON DELETE CASCADE,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    source TEXT NOT NULL CHECK (source IN ('gsc_web','gsc_ai','ai_observed')),
    engine TEXT NOT NULL CHECK (engine IN ('google_ai','chatgpt','gemini','perplexity','other')),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL CHECK (period_end >= period_start),
    country TEXT NOT NULL DEFAULT '',
    device TEXT NOT NULL,
    row_count INT NOT NULL CHECK (row_count BETWEEN 1 AND 5000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (report_id, payload_hash)
);
CREATE INDEX geo_visibility_imports_report_idx ON geo_visibility_imports(report_id, created_at DESC, id);
CREATE TABLE geo_visibility_rows (
    import_id UUID NOT NULL REFERENCES geo_visibility_imports(id) ON DELETE CASCADE,
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 5000),
    observation JSONB NOT NULL CHECK (jsonb_typeof(observation) = 'object'),
    PRIMARY KEY (import_id, ordinal)
);

-- +goose Down
DROP TABLE geo_visibility_rows;
DROP TABLE geo_visibility_imports;
