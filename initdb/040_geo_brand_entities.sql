-- +goose Up
ALTER TABLE geo_profiles
    ADD COLUMN brand_entity JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(brand_entity)='object' AND octet_length(brand_entity::TEXT)<=32768);

-- +goose Down
ALTER TABLE geo_profiles DROP COLUMN brand_entity;
