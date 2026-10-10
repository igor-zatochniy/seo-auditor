-- +goose Up
ALTER TABLE geo_reports ADD CONSTRAINT geo_reports_id_source_unique UNIQUE (id, source_run_id);
CREATE TABLE geo_target_decisions (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL UNIQUE,
    report_id UUID NOT NULL,
    ordinal INT NOT NULL,
    source_run_id UUID NOT NULL,
    target_id BIGINT,
    selected_result JSONB,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (report_id, ordinal) REFERENCES geo_query_results(report_id, ordinal) ON DELETE CASCADE,
    FOREIGN KEY (report_id, source_run_id) REFERENCES geo_reports(id, source_run_id) ON DELETE CASCADE,
    FOREIGN KEY (source_run_id, target_id) REFERENCES audit_results(run_id, target_id) ON DELETE CASCADE,
    CHECK ((target_id IS NULL AND selected_result IS NULL) OR
           (target_id IS NOT NULL AND jsonb_typeof(selected_result) = 'object'))
);
CREATE INDEX geo_target_decisions_latest_idx ON geo_target_decisions(report_id, ordinal, sequence DESC);

-- +goose Down
DROP TABLE geo_target_decisions;
ALTER TABLE geo_reports DROP CONSTRAINT geo_reports_id_source_unique;
