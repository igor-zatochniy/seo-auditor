-- +goose Up
ALTER TABLE audit_results
    ADD COLUMN title_char_count INTEGER CHECK (title_char_count >= 0),
    ADD COLUMN title_width_px INTEGER CHECK (title_width_px >= 0),
    ADD COLUMN description_char_count INTEGER CHECK (description_char_count >= 0),
    ADD COLUMN description_width_px INTEGER CHECK (description_width_px >= 0),
    ADD COLUMN description_mobile_status VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN serp_width_model VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN serp_width_approximate BOOLEAN NOT NULL DEFAULT FALSE;

-- Historical results retain their original character-based statuses; NULL means unmeasured.

-- +goose Down
ALTER TABLE audit_results
    DROP COLUMN serp_width_approximate,
    DROP COLUMN serp_width_model,
    DROP COLUMN description_mobile_status,
    DROP COLUMN description_width_px,
    DROP COLUMN description_char_count,
    DROP COLUMN title_width_px,
    DROP COLUMN title_char_count;
