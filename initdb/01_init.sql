-- Черга URL для етичного сканування.
CREATE TABLE IF NOT EXISTS pages_to_scan (
    id SERIAL PRIMARY KEY,
    url VARCHAR(2048) NOT NULL UNIQUE,
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Результати розширеного SEO-аудиту.
CREATE TABLE IF NOT EXISTS seo_results (
    id SERIAL PRIMARY KEY,
    url VARCHAR(2048) NOT NULL UNIQUE,
    status_code INT,
    scan_status VARCHAR(32) NOT NULL DEFAULT 'completed',
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    run_id VARCHAR(64) NOT NULL DEFAULT '',
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
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Ідемпотентне оновлення outcome-полів під час ручного застосування до наявної схеми.
ALTER TABLE seo_results ALTER COLUMN status_code DROP NOT NULL;
ALTER TABLE seo_results ADD COLUMN IF NOT EXISTS scan_status VARCHAR(32) NOT NULL DEFAULT 'completed';
ALTER TABLE seo_results ADD COLUMN IF NOT EXISTS error_code VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE seo_results ADD COLUMN IF NOT EXISTS error_message TEXT NOT NULL DEFAULT '';
ALTER TABLE seo_results ADD COLUMN IF NOT EXISTS robots_outcome VARCHAR(32) NOT NULL DEFAULT 'not_checked';
ALTER TABLE seo_results ADD COLUMN IF NOT EXISTS run_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE seo_results ALTER COLUMN is_self_canonical SET DEFAULT FALSE;

-- Індекси для швидкого пошуку та аналітичних запитів.
CREATE INDEX IF NOT EXISTS idx_pages_to_scan_active ON pages_to_scan(is_active);
CREATE INDEX IF NOT EXISTS idx_seo_results_status ON seo_results(status_code);
CREATE INDEX IF NOT EXISTS idx_seo_results_scan_status ON seo_results(scan_status);
CREATE INDEX IF NOT EXISTS idx_seo_results_run_status ON seo_results(run_id, scan_status);

-- Початковий набір URL для перевірки локального запуску через Docker Compose.
INSERT INTO pages_to_scan (url, is_active) VALUES
('https://go.dev', true),
('https://golang.org', true),
('https://github.com', true)
ON CONFLICT (url) DO NOTHING;
