-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

ALTER TABLE audit_results
    ADD COLUMN IF NOT EXISTS fingerprint_key_id VARCHAR(32) NOT NULL DEFAULT 'legacy';

ALTER TABLE audit_run_targets
    ADD COLUMN IF NOT EXISTS request_url_cleared_at TIMESTAMP WITH TIME ZONE;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION seo_auditor_redact_legacy_url(raw TEXT)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    no_fragment TEXT;
    query_start INT;
BEGIN
    IF raw IS NULL OR raw = '' THEN
        RETURN raw;
    END IF;

    no_fragment := regexp_replace(raw, '#.*$', '');
    query_start := position('?' in no_fragment);
    IF query_start = 0 THEN
        RETURN no_fragment;
    END IF;

    RETURN substring(no_fragment from 1 for query_start) || '[QUERY_REDACTED]';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION seo_auditor_redact_legacy_text(raw TEXT)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
BEGIN
    IF raw IS NULL OR raw = '' THEN
        RETURN raw;
    END IF;

    RETURN regexp_replace(
        regexp_replace(raw, '#[^[:space:]"''<>]*', '', 'g'),
        '\?[^[:space:]"''<>]*',
        '?[QUERY_REDACTED]',
        'g'
    );
END
$$;
-- +goose StatementEnd

UPDATE audit_results
SET safe_url = seo_auditor_redact_legacy_url(safe_url)
WHERE safe_url LIKE '%?%' OR safe_url LIKE '%#%';

UPDATE audit_results
SET redirect_url = seo_auditor_redact_legacy_url(redirect_url)
WHERE redirect_url LIKE '%?%' OR redirect_url LIKE '%#%';

UPDATE audit_results
SET canonical_url = seo_auditor_redact_legacy_url(canonical_url)
WHERE canonical_url LIKE '%?%' OR canonical_url LIKE '%#%';

UPDATE audit_results
SET og_image = seo_auditor_redact_legacy_url(og_image)
WHERE og_image LIKE '%?%' OR og_image LIKE '%#%';

UPDATE audit_results
SET error_message = LEFT(seo_auditor_redact_legacy_text(error_message), 1000)
WHERE error_message LIKE '%?%' OR error_message LIKE '%#%';

UPDATE audit_run_targets AS target
SET request_url = '',
    request_url_cleared_at = CURRENT_TIMESTAMP
FROM audit_runs AS run
WHERE target.run_id = run.id
  AND run.status <> 'running'
  AND target.request_url <> '';

DROP FUNCTION seo_auditor_redact_legacy_text(TEXT);
DROP FUNCTION seo_auditor_redact_legacy_url(TEXT);

CREATE INDEX IF NOT EXISTS idx_audit_results_fingerprint_key
    ON audit_results(fingerprint_key_id, run_id);

COMMIT;
