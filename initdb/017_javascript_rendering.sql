-- +goose Up
ALTER TABLE audit_runs ADD COLUMN render_javascript BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE audit_results ADD COLUMN rendering JSONB;
ALTER TABLE audit_results ADD CONSTRAINT audit_results_rendering_object
    CHECK (rendering IS NULL OR (jsonb_typeof(rendering) = 'object' AND octet_length(rendering::TEXT) <= 262144));

-- +goose Down
ALTER TABLE audit_results DROP COLUMN rendering;
ALTER TABLE audit_runs DROP COLUMN render_javascript;
