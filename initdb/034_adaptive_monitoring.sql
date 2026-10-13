-- +goose Up
CREATE TABLE audit_adaptive_targets (
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id) ON DELETE CASCADE,
    source_index INT NOT NULL CHECK (source_index BETWEEN 1 AND 10000),
    priority INT NOT NULL DEFAULT 2 CHECK (priority BETWEEN 1 AND 3),
    next_check_at TIMESTAMPTZ NOT NULL,
    stable_checks INT NOT NULL DEFAULT 0 CHECK (stable_checks BETWEEN 0 AND 16),
    consecutive_failures INT NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 1000),
    baseline_run_id UUID,
    baseline_target_id BIGINT,
    PRIMARY KEY(schedule_id,source_index),
    FOREIGN KEY(baseline_run_id,baseline_target_id) REFERENCES audit_results(run_id,target_id) MATCH FULL ON DELETE SET NULL
);
CREATE INDEX audit_adaptive_targets_due ON audit_adaptive_targets(schedule_id,next_check_at,priority DESC,source_index);
CREATE TABLE audit_adaptive_run_targets (
    run_id UUID NOT NULL,
    target_id BIGINT NOT NULL,
    schedule_id UUID NOT NULL,
    source_index INT NOT NULL,
    baseline_run_id UUID,
    baseline_target_id BIGINT,
    PRIMARY KEY(run_id,target_id),
    FOREIGN KEY(run_id,target_id) REFERENCES audit_run_targets(run_id,target_id) ON DELETE CASCADE,
    FOREIGN KEY(schedule_id,source_index) REFERENCES audit_adaptive_targets(schedule_id,source_index) ON DELETE CASCADE,
    FOREIGN KEY(baseline_run_id,baseline_target_id) REFERENCES audit_results(run_id,target_id) MATCH FULL ON DELETE SET NULL
);
CREATE TABLE audit_http_snapshots (
    run_id UUID NOT NULL,
    target_id BIGINT NOT NULL,
    etag TEXT NOT NULL DEFAULT '' CHECK (octet_length(etag)<=2048),
    last_modified TEXT NOT NULL DEFAULT '' CHECK (octet_length(last_modified)<=512),
    data JSONB NOT NULL CHECK (octet_length(data::TEXT)<=262144),
    PRIMARY KEY(run_id,target_id),
    FOREIGN KEY(run_id,target_id) REFERENCES audit_results(run_id,target_id) ON DELETE CASCADE
);
ALTER TABLE audit_results ADD COLUMN http_response_status INT CHECK (http_response_status BETWEEN 100 AND 599);
ALTER TABLE audit_results ADD COLUMN reused_from_run_id UUID REFERENCES audit_runs(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE audit_results DROP COLUMN reused_from_run_id;
ALTER TABLE audit_results DROP COLUMN http_response_status;
DROP TABLE audit_http_snapshots;
DROP TABLE audit_adaptive_run_targets;
DROP TABLE audit_adaptive_targets;
