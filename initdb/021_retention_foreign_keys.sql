-- +goose Up
-- Retention видаляє залежну історію, але не активні профілі чи розклади.
ALTER TABLE geo_profile_runs DROP CONSTRAINT geo_profile_runs_run_id_fkey;
ALTER TABLE geo_profile_runs ADD CONSTRAINT geo_profile_runs_run_id_fkey
    FOREIGN KEY (run_id) REFERENCES audit_runs(id) ON DELETE CASCADE;
ALTER TABLE geo_profile_runs DROP CONSTRAINT geo_profile_runs_baseline_report_id_fkey;
ALTER TABLE geo_profile_runs ADD CONSTRAINT geo_profile_runs_baseline_report_id_fkey
    FOREIGN KEY (baseline_report_id) REFERENCES geo_reports(id) ON DELETE SET NULL;
ALTER TABLE geo_profile_changes DROP CONSTRAINT geo_profile_changes_profile_id_run_id_fkey;
ALTER TABLE geo_profile_changes ADD CONSTRAINT geo_profile_changes_profile_id_run_id_fkey
    FOREIGN KEY (profile_id,run_id) REFERENCES geo_profile_runs(profile_id,run_id) ON DELETE CASCADE;
ALTER TABLE geo_competitor_sets DROP CONSTRAINT geo_competitor_sets_report_id_fkey;
ALTER TABLE geo_competitor_sets ADD CONSTRAINT geo_competitor_sets_report_id_fkey
    FOREIGN KEY (report_id) REFERENCES geo_reports(id) ON DELETE CASCADE;
ALTER TABLE geo_competitor_jobs DROP CONSTRAINT geo_competitor_jobs_set_id_fkey;
ALTER TABLE geo_competitor_jobs ADD CONSTRAINT geo_competitor_jobs_set_id_fkey
    FOREIGN KEY (set_id) REFERENCES geo_competitor_sets(id) ON DELETE CASCADE;
ALTER TABLE geo_competitor_jobs DROP CONSTRAINT geo_competitor_jobs_run_id_fkey;
ALTER TABLE geo_competitor_jobs ADD CONSTRAINT geo_competitor_jobs_run_id_fkey
    FOREIGN KEY (run_id) REFERENCES audit_runs(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE geo_profile_runs DROP CONSTRAINT geo_profile_runs_run_id_fkey;
ALTER TABLE geo_profile_runs ADD CONSTRAINT geo_profile_runs_run_id_fkey FOREIGN KEY (run_id) REFERENCES audit_runs(id);
ALTER TABLE geo_profile_runs DROP CONSTRAINT geo_profile_runs_baseline_report_id_fkey;
ALTER TABLE geo_profile_runs ADD CONSTRAINT geo_profile_runs_baseline_report_id_fkey FOREIGN KEY (baseline_report_id) REFERENCES geo_reports(id);
ALTER TABLE geo_profile_changes DROP CONSTRAINT geo_profile_changes_profile_id_run_id_fkey;
ALTER TABLE geo_profile_changes ADD CONSTRAINT geo_profile_changes_profile_id_run_id_fkey FOREIGN KEY (profile_id,run_id) REFERENCES geo_profile_runs(profile_id,run_id);
ALTER TABLE geo_competitor_sets DROP CONSTRAINT geo_competitor_sets_report_id_fkey;
ALTER TABLE geo_competitor_sets ADD CONSTRAINT geo_competitor_sets_report_id_fkey FOREIGN KEY (report_id) REFERENCES geo_reports(id);
ALTER TABLE geo_competitor_jobs DROP CONSTRAINT geo_competitor_jobs_set_id_fkey;
ALTER TABLE geo_competitor_jobs ADD CONSTRAINT geo_competitor_jobs_set_id_fkey FOREIGN KEY (set_id) REFERENCES geo_competitor_sets(id);
ALTER TABLE geo_competitor_jobs DROP CONSTRAINT geo_competitor_jobs_run_id_fkey;
ALTER TABLE geo_competitor_jobs ADD CONSTRAINT geo_competitor_jobs_run_id_fkey FOREIGN KEY (run_id) REFERENCES audit_runs(id);
