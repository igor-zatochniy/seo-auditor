-- +goose Up
ALTER TABLE geo_reports ADD COLUMN semantic BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE geo_profiles ADD COLUMN semantic BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE geo_profiles DROP COLUMN semantic;
ALTER TABLE geo_reports DROP COLUMN semantic;
