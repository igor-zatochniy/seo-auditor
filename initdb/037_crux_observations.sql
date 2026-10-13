-- +goose Up
CREATE TABLE audit_crux_observations (
    id BIGSERIAL PRIMARY KEY,
    run_id UUID NOT NULL,
    target_id BIGINT NOT NULL,
    device VARCHAR(16) NOT NULL CHECK (device IN ('PHONE','DESKTOP')),
    scope VARCHAR(16) NOT NULL CHECK (scope IN ('url','origin')),
    observed_on DATE NOT NULL DEFAULT CURRENT_DATE,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    report JSONB NOT NULL CHECK (octet_length(report::TEXT) <= 16384),
    FOREIGN KEY (run_id,target_id) REFERENCES audit_results(run_id,target_id) ON DELETE CASCADE,
    UNIQUE(run_id,target_id,device,scope,observed_on)
);
CREATE INDEX audit_crux_target_history ON audit_crux_observations(run_id,target_id,device,scope,id DESC);

-- +goose Down
DROP TABLE audit_crux_observations;
