-- +goose Up
ALTER TABLE audit_site_crawls ADD COLUMN discover_rendered_links BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE audit_site_edges ADD COLUMN discovery_source VARCHAR(16) NOT NULL DEFAULT 'html'
    CHECK (discovery_source IN ('html','rendered','both'));

-- +goose Down
ALTER TABLE audit_site_edges DROP COLUMN discovery_source;
ALTER TABLE audit_site_crawls DROP COLUMN discover_rendered_links;
