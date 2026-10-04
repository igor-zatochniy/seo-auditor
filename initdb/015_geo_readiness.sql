-- +goose Up
ALTER TABLE audit_results ADD COLUMN geo_signals JSONB;

CREATE TABLE geo_reports (
    id UUID PRIMARY KEY,
    source_run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    domain TEXT NOT NULL,
    brand TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    query_count INT NOT NULL CHECK (query_count BETWEEN 1 AND 1000),
    page_count INT NOT NULL CHECK (page_count BETWEEN 1 AND 1000),
    matched_count INT NOT NULL,
    ambiguous_count INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX geo_reports_created_idx ON geo_reports(created_at DESC, id);

CREATE TABLE geo_query_results (
    report_id UUID NOT NULL REFERENCES geo_reports(id) ON DELETE CASCADE,
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 1000),
    result JSONB NOT NULL CHECK (jsonb_typeof(result) = 'object'),
    PRIMARY KEY (report_id, ordinal)
);

CREATE TABLE geo_citation_checks (
    report_id UUID NOT NULL,
    ordinal INT NOT NULL,
    engine TEXT NOT NULL CHECK (engine IN ('google_ai','chatgpt','gemini','perplexity','other')),
    citation TEXT NOT NULL CHECK (citation IN ('unknown','yes','no')),
    brand_mention TEXT NOT NULL CHECK (brand_mention IN ('unknown','yes','no')),
    evidence_url TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (report_id, ordinal, engine),
    FOREIGN KEY (report_id, ordinal) REFERENCES geo_query_results(report_id, ordinal) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE geo_citation_checks;
DROP TABLE geo_query_results;
DROP TABLE geo_reports;
ALTER TABLE audit_results DROP COLUMN geo_signals;
