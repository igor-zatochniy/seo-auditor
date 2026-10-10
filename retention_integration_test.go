//go:build integration

package main

import (
	"testing"
)

func TestRetentionDryRunPreservesActiveAndLatestHistory(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	runs := make([]string, 0, 6)
	for _, status := range []string{"completed", "completed", "completed", "completed", "running", "failed"} {
		cfg.RunID = newWebRunID()
		if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/?token=test-only"}); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, cfg.RunID)
		age := 200
		if len(runs) == 3 || len(runs) == 4 {
			age = 0
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status=$2,finished_at=CURRENT_TIMESTAMP-($3::INT*INTERVAL '1 day') WHERE id=$1`, cfg.RunID, status, age); err != nil {
			t.Fatal(err)
		}
		targetStatus := "completed"
		if status == "running" || status == "failed" {
			targetStatus = status
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status=$2 WHERE run_id=$1`, cfg.RunID, targetStatus); err != nil {
			t.Fatal(err)
		}
	}
	report := newWebRunID()
	if _, err := pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count)
	 VALUES($1,$2,decode(repeat('00',32),'hex'),'example.com','fixture',1,1,0,0)`, report, runs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO geo_query_results(report_id,ordinal,result) VALUES($1,1,'{}')`, report); err != nil {
		t.Fatal(err)
	}
	set := newWebRunID()
	if _, err := pool.Exec(ctx, `INSERT INTO geo_competitor_sets(id,report_id,request_hash,max_pages,fresh_hours)
	 VALUES($1,$2,decode(repeat('00',32),'hex'),1,24)`, set, report); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO geo_competitor_jobs(set_id,ordinal,requested_url,domain,site_mode,run_id,report_id,status)
	 VALUES($1,1,'https://competitor.example/','competitor.example',false,$2,$3,'ready')`, set, runs[1], newWebRunID()); err != nil {
		t.Fatal(err)
	}
	opts := retentionOptions{KeepDays: 90, MaxRuns: 100}
	summary, err := applyDataRetention(ctx, pool, opts)
	if err != nil || !summary.DryRun || summary.DeletedRuns != 0 {
		t.Fatal(summary, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_runs WHERE id=ANY($1::UUID[])`, runs).Scan(&count); err != nil || count != 6 {
		t.Fatal("dry-run modified history", count, err)
	}
	opts.Apply = true
	if _, err = applyDataRetention(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_runs WHERE id=$1)`, runs[0]).Scan(&exists); err != nil || exists {
		t.Fatal("expired parent audit was not removed", exists, err)
	}
	for _, table := range []string{"geo_reports", "geo_query_results", "geo_competitor_sets", "geo_competitor_jobs"} {
		if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("dependent history did not cascade", table, count, err)
		}
	}
	for _, id := range runs[2:] {
		if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_runs WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
			t.Fatal("active, latest or resumable run removed", id, err)
		}
	}
	var raw string
	if err = pool.QueryRow(ctx, `SELECT request_url FROM audit_run_targets WHERE run_id=$1`, runs[5]).Scan(&raw); err != nil || raw == "" {
		t.Fatal("resume source was erased", err)
	}
	if err = pool.QueryRow(ctx, `SELECT request_url FROM audit_run_targets WHERE run_id=$1`, runs[2]).Scan(&raw); err != nil || raw != "" {
		t.Fatal("terminal sensitive URL not cleared", err)
	}
}

func TestRetentionForgetsOnlyExplicitDisabledIdleSchedule(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	_, id := integrationSchedule(t, ctx, pool, cfg, "https://example.com/?token=test-only")
	var unrelatedRuns []string
	for i := 0; i < 3; i++ {
		cfg.RunID = newWebRunID()
		if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://other.example/?token=test-only"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=CURRENT_TIMESTAMP-INTERVAL '200 days' WHERE id=$1`, cfg.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status='completed' WHERE run_id=$1`, cfg.RunID); err != nil {
			t.Fatal(err)
		}
		unrelatedRuns = append(unrelatedRuns, cfg.RunID)
	}
	opts := retentionOptions{KeepDays: 90, MaxRuns: 10, ForgetSchedule: id}
	if _, err := applyDataRetention(ctx, pool, opts); err == nil {
		t.Fatal("enabled schedule accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_schedules SET enabled=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := applyDataRetention(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_schedules WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
		t.Fatal("dry-run forgot schedule", err)
	}
	opts.Apply = true
	if _, err := applyDataRetention(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_schedules WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
		t.Fatal("schedule source not removed", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_runs WHERE id=ANY($1::UUID[])`, unrelatedRuns).Scan(&count); err != nil || count != len(unrelatedRuns) {
		t.Fatal("forget-schedule removed unrelated audits", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_run_targets WHERE run_id=ANY($1::UUID[]) AND request_url<>''`, unrelatedRuns).Scan(&count); err != nil || count != len(unrelatedRuns) {
		t.Fatal("forget-schedule cleared unrelated URLs", count, err)
	}
}

func TestRetentionKeepsReadyGEOBaselinesAfterFailedJobs(t *testing.T) {
	ctx, pool, cfg := webIntegration(t)
	_, scheduleID := integrationSchedule(t, ctx, pool, cfg, "https://example.com/")
	profile := newWebRunID()
	if _, err := pool.Exec(ctx, `INSERT INTO geo_profiles(id,schedule_id,name,domain,queries,request_hash)
	 VALUES($1,$2,'Retained baseline','example.com','["test"]',decode(repeat('00',32),'hex'))`, profile, scheduleID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM geo_profile_runs WHERE profile_id=$1`, profile)
		_, _ = pool.Exec(ctx, `DELETE FROM geo_profiles WHERE id=$1`, profile)
	})
	var runs, reports []string
	for i := 0; i < 6; i++ {
		cfg.RunID = newWebRunID()
		if err := createExplicitAuditRun(ctx, pool, &cfg, []string{"https://example.com/"}); err != nil {
			t.Fatal(err)
		}
		age := 250 - i*20
		if i >= 4 {
			age = 0
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_runs SET status='completed',finished_at=CURRENT_TIMESTAMP-($2::INT*INTERVAL '1 day') WHERE id=$1`, cfg.RunID, age); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status='completed' WHERE run_id=$1`, cfg.RunID); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, cfg.RunID)
		if i >= 4 {
			continue
		}
		report := newWebRunID()
		reports = append(reports, report)
		if _, err := pool.Exec(ctx, `INSERT INTO geo_reports(id,source_run_id,request_hash,domain,model,query_count,page_count,matched_count,ambiguous_count)
		 VALUES($1,$2,decode(repeat('00',32),'hex'),'example.com','fixture',1,1,0,0)`, report, cfg.RunID); err != nil {
			t.Fatal(err)
		}
		status := "ready"
		if i == 3 {
			status = "failed"
		}
		var baseline *string
		if i > 0 {
			baseline = &reports[i-1]
		}
		if _, err := pool.Exec(ctx, `INSERT INTO geo_profile_runs(profile_id,run_id,report_id,scheduled_at,status,baseline_report_id)
		 VALUES($1,$2,$3,CURRENT_TIMESTAMP-($4::INT*INTERVAL '1 day'),$5,$6)`, profile, cfg.RunID, report, age, status, baseline); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := applyDataRetention(ctx, pool, retentionOptions{KeepDays: 90, MaxRuns: 100, Apply: true}); err != nil {
		t.Fatal(err)
	}
	for i, id := range runs {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_runs WHERE id=$1)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		want := i == 1 || i == 2 || i >= 4
		if exists != want {
			t.Fatalf("run %d retained=%v want=%v", i, exists, want)
		}
	}
}
