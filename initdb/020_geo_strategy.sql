-- +goose Up
CREATE TABLE geo_profiles (
    id UUID PRIMARY KEY,
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id),
    name VARCHAR(120) NOT NULL,
    domain VARCHAR(253) NOT NULL,
    brand VARCHAR(120) NOT NULL DEFAULT '',
    queries JSONB NOT NULL CHECK (jsonb_typeof(queries) = 'array' AND jsonb_array_length(queries) BETWEEN 1 AND 1000),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX geo_profiles_schedule_idx ON geo_profiles(schedule_id) WHERE enabled;
CREATE TABLE geo_profile_runs (
    profile_id UUID NOT NULL REFERENCES geo_profiles(id),
    run_id UUID NOT NULL REFERENCES audit_runs(id),
    report_id UUID NOT NULL UNIQUE,
    scheduled_at TIMESTAMPTZ NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed','interrupted')),
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 3),
    baseline_report_id UUID REFERENCES geo_reports(id),
    error_message TEXT NOT NULL DEFAULT '',
    finished_at TIMESTAMPTZ,
    PRIMARY KEY(profile_id,run_id)
);
CREATE INDEX geo_profile_runs_pending_idx ON geo_profile_runs(scheduled_at,profile_id,run_id) WHERE status='pending';
CREATE TABLE geo_profile_changes (
    profile_id UUID NOT NULL,
    run_id UUID NOT NULL,
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 1000),
    kind VARCHAR(64) NOT NULL,
    query TEXT NOT NULL,
    before_value TEXT NOT NULL,
    after_value TEXT NOT NULL,
    PRIMARY KEY(profile_id,run_id,ordinal,kind),
    FOREIGN KEY(profile_id,run_id) REFERENCES geo_profile_runs(profile_id,run_id)
);
CREATE TABLE geo_competitor_sets (
    id UUID PRIMARY KEY,
    report_id UUID NOT NULL REFERENCES geo_reports(id),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    canceled BOOLEAN NOT NULL DEFAULT FALSE,
    max_pages INT NOT NULL CHECK (max_pages BETWEEN 1 AND 50),
    fresh_hours INT NOT NULL CHECK (fresh_hours BETWEEN 1 AND 168),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE geo_competitor_jobs (
    set_id UUID NOT NULL REFERENCES geo_competitor_sets(id),
    ordinal INT NOT NULL CHECK (ordinal BETWEEN 1 AND 5),
    requested_url TEXT NOT NULL,
    domain VARCHAR(253) NOT NULL,
    brand VARCHAR(120) NOT NULL DEFAULT '',
    site_mode BOOLEAN NOT NULL,
    run_id UUID REFERENCES audit_runs(id),
    report_id UUID NOT NULL UNIQUE,
    status VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','scanning','ready','failed','canceled')),
    reused BOOLEAN NOT NULL DEFAULT FALSE,
    attempts INT NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
    error_message TEXT NOT NULL DEFAULT '',
    finished_at TIMESTAMPTZ,
    PRIMARY KEY(set_id,ordinal),
    UNIQUE(set_id,requested_url)
);
CREATE INDEX geo_competitor_pending_idx ON geo_competitor_jobs(set_id,ordinal) WHERE status='pending';
CREATE INDEX geo_competitor_scanning_idx ON geo_competitor_jobs(run_id) WHERE status='scanning';

-- +goose Down
DROP TABLE geo_competitor_jobs;
DROP TABLE geo_competitor_sets;
DROP TABLE geo_profile_changes;
DROP TABLE geo_profile_runs;
DROP TABLE geo_profiles;
