-- +goose Up
-- Знімок запитів і статистика відбору належать запуску, а не поточним налаштуванням профілю.
ALTER TABLE audit_site_crawls ADD COLUMN competitor_selection JSONB;
ALTER TABLE audit_site_crawls ADD CONSTRAINT audit_site_competitor_selection_object
CHECK (competitor_selection IS NULL OR jsonb_typeof(competitor_selection) = 'object');

-- +goose Down
ALTER TABLE audit_site_crawls DROP COLUMN competitor_selection;
