-- +goose Up
-- An existing HMAC cannot be reconstructed without the original secret.
-- Historical imports remain readable; NULL prevents mixing new identities.
ALTER TABLE audit_search_imports ADD COLUMN key_check BYTEA
    CHECK (key_check IS NULL OR octet_length(key_check)=32);

-- +goose Down
ALTER TABLE audit_search_imports DROP COLUMN key_check;
