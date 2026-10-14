-- +goose Up
ALTER TABLE audit_rule_results
    ADD COLUMN observed_value TEXT NOT NULL DEFAULT '',
    ADD COLUMN consecutive_failures SMALLINT NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 10),
    ADD COLUMN notified BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE audit_change_events ADD COLUMN rule_id UUID REFERENCES audit_custom_rules(id) ON DELETE SET NULL;
ALTER TABLE audit_custom_rules ADD COLUMN last_notified_at TIMESTAMPTZ;

UPDATE audit_change_events e SET rule_id=c.id
FROM audit_custom_rules c
WHERE e.kind='rule_' || replace(c.id::TEXT,'-','');

UPDATE audit_rule_results p SET notified=TRUE,consecutive_failures=1
WHERE p.state='fail' AND EXISTS (
    SELECT 1 FROM audit_change_events e
    WHERE e.run_id=p.run_id AND e.target_id=p.target_id AND e.rule_id=p.rule_id
);

UPDATE audit_custom_rules c SET last_notified_at=(
    SELECT MAX(e.created_at) FROM audit_change_events e WHERE e.rule_id=c.id
)
WHERE EXISTS (SELECT 1 FROM audit_change_events e WHERE e.rule_id=c.id);

CREATE TABLE audit_rule_exceptions (
    schedule_id UUID NOT NULL REFERENCES audit_schedules(id) ON DELETE CASCADE,
    rule_id UUID NOT NULL REFERENCES audit_custom_rules(id) ON DELETE CASCADE,
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    safe_url TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 250),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY(schedule_id,rule_id,fingerprint)
);
CREATE INDEX audit_rule_exceptions_active ON audit_rule_exceptions(schedule_id,expires_at);
CREATE INDEX audit_change_events_rule_last_notified ON audit_change_events(rule_id,created_at DESC) WHERE rule_id IS NOT NULL;

-- +goose Down
DROP TABLE audit_rule_exceptions;
DROP INDEX audit_change_events_rule_last_notified;
ALTER TABLE audit_custom_rules DROP COLUMN last_notified_at;
ALTER TABLE audit_change_events DROP COLUMN rule_id;
ALTER TABLE audit_rule_results DROP COLUMN notified, DROP COLUMN consecutive_failures, DROP COLUMN observed_value;
