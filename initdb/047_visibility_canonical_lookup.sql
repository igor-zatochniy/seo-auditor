-- +goose Up
-- +goose NO TRANSACTION

-- Пакетний lookup canonical не завантажує весь run у RAM і не сканує його на кожен URL.
-- Невалідний interrupted index очищає application preflight; валідний повторно використовується.
CREATE INDEX CONCURRENTLY IF NOT EXISTS audit_results_visibility_canonical_idx
    ON audit_results USING GIN ((url_signals->'html'->'fingerprints') jsonb_path_ops)
    WHERE scan_status='completed' AND status_code IS NOT NULL;

-- +goose Down
-- +goose NO TRANSACTION

DROP INDEX CONCURRENTLY IF EXISTS audit_results_visibility_canonical_idx;
