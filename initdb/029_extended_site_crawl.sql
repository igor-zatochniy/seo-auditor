-- +goose Up
ALTER TABLE audit_site_crawls DROP CONSTRAINT audit_site_crawls_max_pages_check;
ALTER TABLE audit_site_crawls ADD CONSTRAINT audit_site_crawls_max_pages_check
    CHECK (max_pages BETWEEN 1 AND 20000);
ALTER TABLE audit_schedules DROP CONSTRAINT audit_schedules_target_count_check;
ALTER TABLE audit_schedules ADD CONSTRAINT audit_schedules_target_count_check
    CHECK (target_count BETWEEN 1 AND 20000);

-- +goose Down
-- Відкат не має приховано видаляти або змінювати налаштування великих аудитів.
ALTER TABLE audit_schedules DROP CONSTRAINT audit_schedules_target_count_check;
ALTER TABLE audit_schedules ADD CONSTRAINT audit_schedules_target_count_check
    CHECK (target_count BETWEEN 1 AND 10000);
ALTER TABLE audit_site_crawls DROP CONSTRAINT audit_site_crawls_max_pages_check;
ALTER TABLE audit_site_crawls ADD CONSTRAINT audit_site_crawls_max_pages_check
    CHECK (max_pages BETWEEN 1 AND 1000);
