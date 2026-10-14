-- +goose Up
ALTER TABLE geo_visibility_imports
    DROP CONSTRAINT geo_visibility_imports_source_check;
ALTER TABLE geo_visibility_imports
    ADD CONSTRAINT geo_visibility_imports_source_check
    CHECK (source IN ('gsc_web','gsc_ai','ai_observed','ga4'));
CREATE INDEX geo_visibility_imports_domain_source_period
    ON geo_visibility_imports(report_id,source,country,device,period_start,period_end,created_at DESC);

-- +goose Down
DROP INDEX geo_visibility_imports_domain_source_period;
ALTER TABLE geo_visibility_imports
    DROP CONSTRAINT geo_visibility_imports_source_check;
ALTER TABLE geo_visibility_imports
    ADD CONSTRAINT geo_visibility_imports_source_check
    CHECK (source IN ('gsc_web','gsc_ai','ai_observed')) NOT VALID;
