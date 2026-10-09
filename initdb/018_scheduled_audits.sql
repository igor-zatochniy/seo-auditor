-- +goose Up
CREATE TABLE audit_schedules (
    id UUID PRIMARY KEY,
    name VARCHAR(120) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    interval_hours INT NOT NULL CHECK (interval_hours BETWEEN 1 AND 720),
    next_run_at TIMESTAMPTZ NOT NULL,
    source JSONB NOT NULL CHECK (jsonb_typeof(source) = 'object' AND octet_length(source::TEXT) <= 16777216),
    request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
    source_label TEXT NOT NULL,
    target_count INT NOT NULL CHECK (target_count BETWEEN 1 AND 10000),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX audit_schedules_due ON audit_schedules(next_run_at, id) WHERE enabled;

CREATE TABLE audit_schedule_runs (
    run_id UUID PRIMARY KEY REFERENCES audit_runs(id) ON DELETE CASCADE,
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id) ON DELETE CASCADE,
    baseline_run_id UUID REFERENCES audit_runs(id) ON DELETE SET NULL,
    key_check BYTEA NOT NULL CHECK (octet_length(key_check) = 32),
    scheduled_at TIMESTAMPTZ NOT NULL,
    comparison_status VARCHAR(32) NOT NULL DEFAULT 'pending'
        CHECK (comparison_status IN ('pending', 'baseline', 'compared', 'interrupted')),
    compared_at TIMESTAMPTZ,
    change_count INT NOT NULL DEFAULT 0
);
CREATE INDEX audit_schedule_runs_history ON audit_schedule_runs(schedule_id, scheduled_at DESC, run_id);
CREATE INDEX audit_schedule_runs_pending ON audit_schedule_runs(scheduled_at, run_id) WHERE comparison_status = 'pending';

CREATE TABLE audit_change_events (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES audit_schedule_runs(run_id) ON DELETE CASCADE,
    target_id BIGINT NOT NULL,
    safe_url TEXT NOT NULL,
    kind VARCHAR(40) NOT NULL,
    severity VARCHAR(16) NOT NULL CHECK (severity IN ('critical', 'warning')),
    before_value TEXT NOT NULL DEFAULT '',
    after_value TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at TIMESTAMPTZ,
    UNIQUE (run_id, target_id, kind)
);
CREATE INDEX audit_change_events_unread ON audit_change_events(id DESC) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE audit_change_events;
DROP TABLE audit_schedule_runs;
DROP TABLE audit_schedules;
