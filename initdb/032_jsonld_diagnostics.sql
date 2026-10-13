-- +goose Up
ALTER TABLE audit_results ADD COLUMN structured_data JSONB;
ALTER TABLE audit_results ADD CONSTRAINT audit_results_structured_data_size
    CHECK (structured_data IS NULL OR octet_length(structured_data::TEXT) <= 65536);

-- +goose Down
ALTER TABLE audit_results DROP COLUMN structured_data;
