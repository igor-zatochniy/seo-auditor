-- +goose Up
ALTER TABLE audit_results ADD COLUMN international JSONB;
ALTER TABLE audit_results ADD CONSTRAINT audit_results_international_budget
    CHECK (international IS NULL OR octet_length(international::TEXT) <= 160000);
CREATE INDEX audit_results_run_fingerprint_idx ON audit_results(run_id, target_fingerprint);
CREATE TABLE audit_sitemap_hreflang (
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    page_fingerprint BYTEA NOT NULL CHECK (octet_length(page_fingerprint)=32),
    evidence JSONB NOT NULL CHECK (octet_length(evidence::TEXT)<=160000),
    PRIMARY KEY(run_id,page_fingerprint)
);

-- +goose Down
DROP TABLE audit_sitemap_hreflang;
DROP INDEX audit_results_run_fingerprint_idx;
ALTER TABLE audit_results DROP CONSTRAINT audit_results_international_budget;
ALTER TABLE audit_results DROP COLUMN international;
