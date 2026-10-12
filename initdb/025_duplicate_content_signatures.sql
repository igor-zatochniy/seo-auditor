-- +goose Up
-- Старі результати залишаються без підпису: короткий GEO-фрагмент не є повним контентом.
ALTER TABLE audit_results ADD COLUMN content_signature JSONB;
ALTER TABLE audit_results ADD CONSTRAINT audit_results_content_signature_object
CHECK (content_signature IS NULL OR jsonb_typeof(content_signature) = 'object');

-- +goose Down
ALTER TABLE audit_results DROP COLUMN content_signature;
