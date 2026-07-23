-- +goose Up
-- +goose NO TRANSACTION

BEGIN;

CREATE TABLE IF NOT EXISTS audit_runs (
    id UUID PRIMARY KEY,
    started_at TIMESTAMP WITH TIME ZONE NOT NULL,
    finished_at TIMESTAMP WITH TIME ZONE,
    status VARCHAR(32) NOT NULL,
    total_urls INT NOT NULL DEFAULT 0,
    successful_urls INT NOT NULL DEFAULT 0,
    failed_urls INT NOT NULL DEFAULT 0,
    CONSTRAINT audit_runs_status_check CHECK (status IN ('running', 'completed', 'completed_with_errors', 'failed', 'canceled')),
    CONSTRAINT audit_runs_counters_check CHECK (
        total_urls >= 0
        AND successful_urls >= 0
        AND failed_urls >= 0
        AND successful_urls + failed_urls <= total_urls
    )
);

ALTER TABLE audit_runs
    DROP CONSTRAINT IF EXISTS audit_runs_status_check;

UPDATE audit_runs
SET status = 'completed_with_errors'
WHERE status = 'partial';

ALTER TABLE audit_runs
    ADD CONSTRAINT audit_runs_status_check
    CHECK (status IN ('running', 'completed', 'completed_with_errors', 'failed', 'canceled'));

CREATE TABLE IF NOT EXISTS audit_results (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    url VARCHAR(2048) NOT NULL,
    status_code INT,
    scan_status VARCHAR(32) NOT NULL DEFAULT 'completed',
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    is_redirect BOOLEAN DEFAULT FALSE,
    redirect_url VARCHAR(2048) DEFAULT '',
    title VARCHAR(500) DEFAULT '',
    title_status VARCHAR(50) DEFAULT '',
    description TEXT DEFAULT '',
    description_status VARCHAR(50) DEFAULT '',
    h1 VARCHAR(1000) DEFAULT '',
    h1_count INT DEFAULT 0,
    h2_to_h6_status TEXT DEFAULT '',
    og_title VARCHAR(500) DEFAULT '',
    og_description TEXT DEFAULT '',
    og_image VARCHAR(2048) DEFAULT '',
    twitter_card VARCHAR(100) DEFAULT '',
    internal_links_count INT DEFAULT 0,
    external_links_count INT DEFAULT 0,
    links_count INT DEFAULT 0,
    canonical_url VARCHAR(2048) DEFAULT '',
    is_self_canonical BOOLEAN DEFAULT FALSE,
    meta_robots VARCHAR(200) DEFAULT '',
    x_robots_tag VARCHAR(200) DEFAULT '',
    robots_allowed BOOLEAN DEFAULT TRUE,
    robots_outcome VARCHAR(32) NOT NULL DEFAULT 'not_checked',
    has_json_ld BOOLEAN DEFAULT FALSE,
    has_viewport BOOLEAN DEFAULT FALSE,
    total_images INT DEFAULT 0,
    images_missing_alt INT DEFAULT 0,
    word_count INT DEFAULT 0,
    duration_ms INT DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT audit_results_run_url_key UNIQUE (run_id, url)
);

-- Перенесення результатів зі старої таблиці без втрати попередніх аудитів.
-- +goose StatementBegin
DO $$
BEGIN
    IF to_regclass('seo_results') IS NULL THEN
        RETURN;
    END IF;

    CREATE TEMPORARY TABLE legacy_audit_run_map (
        legacy_run_id VARCHAR(64) PRIMARY KEY,
        new_run_id UUID NOT NULL UNIQUE
    ) ON COMMIT DROP;

    INSERT INTO legacy_audit_run_map (legacy_run_id, new_run_id)
    SELECT DISTINCT
        run_id,
        CASE
            WHEN REPLACE(run_id, '-', '') ~* '^[0-9a-f]{32}$' THEN run_id::UUID
            ELSE MD5('seo-auditor-legacy-run:' || run_id)::UUID
        END
    FROM seo_results;

    INSERT INTO audit_runs (
        id,
        started_at,
        finished_at,
        status,
        total_urls,
        successful_urls,
        failed_urls
    )
    SELECT
        run_map.new_run_id,
        COALESCE(MIN(result.created_at), CURRENT_TIMESTAMP),
        COALESCE(MAX(result.created_at), CURRENT_TIMESTAMP),
        CASE
            WHEN COUNT(*) FILTER (WHERE result.scan_status = 'failed') > 0 THEN 'completed_with_errors'
            ELSE 'completed'
        END,
        COUNT(*)::INT,
        COUNT(*) FILTER (WHERE result.scan_status <> 'failed')::INT,
        COUNT(*) FILTER (WHERE result.scan_status = 'failed')::INT
    FROM seo_results AS result
    JOIN legacy_audit_run_map AS run_map ON run_map.legacy_run_id = result.run_id
    GROUP BY run_map.new_run_id
    ON CONFLICT (id) DO NOTHING;

    INSERT INTO audit_results (
        run_id, url, status_code, scan_status, error_code, error_message,
        is_redirect, redirect_url, title, title_status, description, description_status,
        h1, h1_count, h2_to_h6_status, og_title, og_description, og_image, twitter_card,
        internal_links_count, external_links_count, links_count, canonical_url, is_self_canonical,
        meta_robots, x_robots_tag, robots_allowed, robots_outcome, has_json_ld, has_viewport,
        total_images, images_missing_alt, word_count, duration_ms, created_at
    )
    SELECT
        run_map.new_run_id, result.url, result.status_code, result.scan_status,
        result.error_code, result.error_message, result.is_redirect, result.redirect_url,
        result.title, result.title_status, result.description, result.description_status,
        result.h1, result.h1_count, result.h2_to_h6_status, result.og_title,
        result.og_description, result.og_image, result.twitter_card,
        result.internal_links_count, result.external_links_count, result.links_count,
        result.canonical_url, result.is_self_canonical, result.meta_robots,
        result.x_robots_tag, result.robots_allowed, result.robots_outcome,
        result.has_json_ld, result.has_viewport, result.total_images,
        result.images_missing_alt, result.word_count, result.duration_ms,
        COALESCE(result.created_at, CURRENT_TIMESTAMP)
    FROM seo_results AS result
    JOIN legacy_audit_run_map AS run_map ON run_map.legacy_run_id = result.run_id
    ON CONFLICT (run_id, url) DO NOTHING;

    DROP TABLE seo_results;
END
$$;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS idx_audit_runs_status_started
    ON audit_runs(status, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_results_status
    ON audit_results(status_code);
CREATE INDEX IF NOT EXISTS idx_audit_results_scan_status
    ON audit_results(scan_status);
CREATE INDEX IF NOT EXISTS idx_audit_results_run_status
    ON audit_results(run_id, scan_status);
CREATE INDEX IF NOT EXISTS idx_audit_results_url_created
    ON audit_results(url, created_at DESC);

COMMIT;
