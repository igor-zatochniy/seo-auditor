-- +goose Up
CREATE TABLE audit_site_crawls (
    run_id UUID PRIMARY KEY REFERENCES audit_runs(id) ON DELETE CASCADE,
    origin TEXT NOT NULL,
    root_safe_url TEXT NOT NULL,
    key_check BYTEA NOT NULL CHECK (octet_length(key_check) = 32),
    max_pages INT NOT NULL CHECK (max_pages BETWEEN 1 AND 1000),
    max_depth INT NOT NULL CHECK (max_depth BETWEEN 0 AND 10),
    use_sitemaps BOOLEAN NOT NULL,
    sitemap_state TEXT NOT NULL DEFAULT 'pending' CHECK (sitemap_state IN ('pending','done','partial','disabled')),
    discovery_warning TEXT NOT NULL DEFAULT '',
    limit_reached BOOLEAN NOT NULL DEFAULT FALSE,
    graph_ready BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE audit_site_nodes (
    run_id UUID NOT NULL REFERENCES audit_site_crawls(run_id) ON DELETE CASCADE,
    target_id BIGINT NOT NULL,
    url_fingerprint BYTEA NOT NULL CHECK (octet_length(url_fingerprint) = 32),
    safe_url TEXT NOT NULL,
    frontier_depth INT NOT NULL CHECK (frontier_depth BETWEEN 0 AND 1010),
    in_sitemap BOOLEAN NOT NULL DEFAULT FALSE,
    crawl_depth INT,
    internal_inlinks_count INT,
    internal_outlinks_count INT,
    broken_internal_links INT,
    redirecting_internal_links INT,
    orphan_candidate BOOLEAN,
    links_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (run_id, target_id),
    UNIQUE (run_id, url_fingerprint),
    FOREIGN KEY (run_id, target_id) REFERENCES audit_run_targets(run_id, target_id) ON DELETE CASCADE
);
CREATE INDEX audit_site_nodes_frontier_idx ON audit_site_nodes(run_id, frontier_depth, target_id);
CREATE TABLE audit_site_edges (
    run_id UUID NOT NULL,
    from_target_id BIGINT NOT NULL,
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 256),
    to_fingerprint BYTEA NOT NULL CHECK (octet_length(to_fingerprint) = 32),
    safe_url TEXT NOT NULL,
    anchor_text TEXT NOT NULL DEFAULT '',
    is_internal BOOLEAN NOT NULL,
    nofollow BOOLEAN NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('link','redirect')),
    PRIMARY KEY (run_id, from_target_id, ordinal),
    FOREIGN KEY (run_id, from_target_id) REFERENCES audit_site_nodes(run_id, target_id) ON DELETE CASCADE
);
CREATE INDEX audit_site_edges_destination_idx ON audit_site_edges(run_id, to_fingerprint);

-- +goose Down
DROP TABLE audit_site_edges;
DROP TABLE audit_site_nodes;
DROP TABLE audit_site_crawls;
