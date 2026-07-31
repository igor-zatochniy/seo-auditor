-- +goose Up
-- +goose NO TRANSACTION

-- Підтримує keyset-читання активних цілей без сортування всієї таблиці.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pages_to_scan_active_id
    ON pages_to_scan(id)
    WHERE is_active = TRUE;

-- Індекс зменшується після кожного batch очищення та не змушує повторно
-- переглядати вже очищені URL завершених запусків.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_run_targets_retained_url
    ON audit_run_targets(run_id, target_id)
    WHERE request_url <> '';

-- +goose Down
-- +goose NO TRANSACTION

DROP INDEX CONCURRENTLY IF EXISTS idx_audit_run_targets_retained_url;
DROP INDEX CONCURRENTLY IF EXISTS idx_pages_to_scan_active_id;
