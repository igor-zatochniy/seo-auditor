-- +goose Up
ALTER TABLE audit_results ADD COLUMN url_signals JSONB
    CHECK (url_signals IS NULL OR octet_length(url_signals::TEXT) <= 65536);

-- +goose Down
ALTER TABLE audit_results DROP COLUMN url_signals;
