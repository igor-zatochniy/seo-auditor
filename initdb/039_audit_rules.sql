-- +goose Up
CREATE TABLE audit_custom_rules (
    id UUID PRIMARY KEY,
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id) ON DELETE CASCADE,
    revision INT NOT NULL DEFAULT 1 CHECK (revision>0),
    definition JSONB NOT NULL CHECK (octet_length(definition::TEXT)<=4096),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX audit_custom_rules_schedule ON audit_custom_rules(schedule_id,id);
CREATE TABLE audit_rule_runs (
    run_id UUID PRIMARY KEY REFERENCES audit_runs(id) ON DELETE CASCADE,
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id) ON DELETE CASCADE,
    rules JSONB NOT NULL CHECK (jsonb_typeof(rules)='array' AND octet_length(rules::TEXT)<=131072),
    key_check BYTEA NOT NULL CHECK (octet_length(key_check)=32),
    status VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','done','interrupted')),
    after_target BIGINT NOT NULL DEFAULT 0,
    checked INT NOT NULL DEFAULT 0,
    automatic BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX audit_rule_runs_pending ON audit_rule_runs(created_at,run_id) WHERE status='pending';
CREATE TABLE audit_rule_results (
    run_id UUID NOT NULL REFERENCES audit_rule_runs(run_id) ON DELETE CASCADE,
    rule_id UUID NOT NULL,
    revision INT NOT NULL,
    target_id BIGINT NOT NULL,
    fingerprint BYTEA NOT NULL,
    safe_url TEXT NOT NULL,
    state VARCHAR(16) NOT NULL CHECK (state IN ('pass','fail','unknown')),
    evidence TEXT NOT NULL CHECK (length(evidence)<=1000),
    PRIMARY KEY(run_id,rule_id,target_id),
    FOREIGN KEY(run_id,target_id) REFERENCES audit_results(run_id,target_id) ON DELETE CASCADE
);
CREATE INDEX audit_rule_results_baseline ON audit_rule_results(rule_id,revision,fingerprint,run_id);

-- +goose Down
DROP TABLE audit_rule_results;
DROP TABLE audit_rule_runs;
DROP TABLE audit_custom_rules;
