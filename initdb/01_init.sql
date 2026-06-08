-- URL queue for ethical crawling.
CREATE TABLE IF NOT EXISTS pages_to_scan (
    id SERIAL PRIMARY KEY,
    url VARCHAR(2048) NOT NULL UNIQUE,
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- SEO audit results.
CREATE TABLE IF NOT EXISTS seo_results (
    id SERIAL PRIMARY KEY,
    url VARCHAR(2048) NOT NULL UNIQUE,
    status_code INT,
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
    has_json_ld BOOLEAN DEFAULT FALSE,
    has_viewport BOOLEAN DEFAULT FALSE,
    total_images INT DEFAULT 0,
    images_missing_alt INT DEFAULT 0,
    word_count INT DEFAULT 0,
    duration_ms INT DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

-- Basic indexes for queue and result lookup.
CREATE INDEX IF NOT EXISTS idx_pages_to_scan_active ON pages_to_scan(is_active);
CREATE INDEX IF NOT EXISTS idx_seo_results_status ON seo_results(status_code);

-- Seed URLs for local verification.
INSERT INTO pages_to_scan (url, is_active) VALUES
('https://go.dev', true),
('https://golang.org', true),
('https://github.com', true)
ON CONFLICT (url) DO NOTHING;
