package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type retentionOptions struct {
	KeepDays       int
	MaxRuns        int
	Apply          bool
	ForgetSchedule string
}

type retentionSummary struct {
	DryRun             bool `json:"dry_run"`
	EligibleRuns       int  `json:"eligible_runs"`
	DeletedRuns        int  `json:"deleted_runs"`
	ClearedTargetURLs  int  `json:"cleared_target_urls"`
	ForgottenSchedules int  `json:"forgotten_schedules"`
}

// Активні/відновлювані запуски, останні дві точки моніторингу й джерела
// незавершеного порівняння не підлягають видаленню.
const retentionCandidateSQL = `SELECT r.id::TEXT FROM audit_runs r
 WHERE r.status IN ('completed','completed_with_errors','canceled')
 AND r.finished_at < $1
 AND r.id NOT IN (SELECT id FROM audit_runs WHERE status IN ('completed','completed_with_errors','canceled') ORDER BY finished_at DESC NULLS LAST,id DESC LIMIT 2)
 AND NOT EXISTS (SELECT 1 FROM audit_run_targets t WHERE t.run_id=r.id AND t.status IN ('pending','running','abandoned'))
 AND NOT EXISTS (SELECT 1 FROM audit_schedule_runs s WHERE s.comparison_status='pending' AND (s.run_id=r.id OR s.baseline_run_id=r.id))
 AND NOT EXISTS (SELECT 1 FROM audit_schedule_runs s WHERE s.run_id=r.id AND r.id IN
   (SELECT h.run_id FROM audit_schedule_runs h WHERE h.schedule_id=s.schedule_id AND h.key_check=s.key_check
    AND h.comparison_status IN ('baseline','compared') ORDER BY h.scheduled_at DESC,h.run_id DESC LIMIT 2))
 AND NOT EXISTS (SELECT 1 FROM geo_profile_runs p WHERE p.status='pending' AND
   (p.run_id=r.id OR p.baseline_report_id IN (SELECT id FROM geo_reports WHERE source_run_id=r.id)))
 AND NOT EXISTS (SELECT 1 FROM geo_profile_runs p WHERE p.run_id=r.id AND r.id IN
   (SELECT h.run_id FROM geo_profile_runs h JOIN geo_reports g ON g.id=h.report_id WHERE h.profile_id=p.profile_id AND h.status='ready'
    AND g.model=(SELECT model FROM geo_reports WHERE id=p.report_id) ORDER BY h.scheduled_at DESC,h.run_id DESC LIMIT 2))
 AND NOT EXISTS (SELECT 1 FROM geo_competitor_jobs j WHERE j.run_id=r.id)
 AND NOT EXISTS (SELECT 1 FROM geo_competitor_sets s JOIN geo_reports g ON g.id=s.report_id
   JOIN geo_competitor_jobs j ON j.set_id=s.id WHERE g.source_run_id=r.id AND j.status IN ('pending','scanning'))
 ORDER BY r.finished_at,r.id LIMIT $2`

func validateRetentionOptions(opts retentionOptions) error {
	if opts.KeepDays < 7 || opts.KeepDays > 3650 || opts.MaxRuns < 1 || opts.MaxRuns > 1000 {
		return fmt.Errorf("keep-days: 7–3650; max-runs: 1–1000")
	}
	if opts.ForgetSchedule != "" && !runIDPattern.MatchString(opts.ForgetSchedule) {
		return fmt.Errorf("forget-schedule має бути UUID вимкненого розкладу")
	}
	return nil
}

func runRetentionCommand(args []string) int {
	flags := flag.NewFlagSet("retention", flag.ContinueOnError)
	opts := retentionOptions{}
	flags.IntVar(&opts.KeepDays, "keep-days", 90, "Кількість днів зберігання")
	flags.IntVar(&opts.MaxRuns, "max-runs", 100, "Максимум видалених запусків за один виклик")
	flags.BoolVar(&opts.Apply, "apply", false, "Застосувати видалення після backup; типово лише суха перевірка")
	flags.StringVar(&opts.ForgetSchedule, "forget-schedule", "", "Забути вихідні URL конкретного вимкненого розкладу та його моніторинг")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitFatal
	}
	if err := validateRetentionOptions(opts); err != nil {
		slog.Error("Некоректні параметри retention", "error", err)
		return exitFatal
	}
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("Некоректна конфігурація retention", "error", err)
		return exitFatal
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("Не вдалося відкрити БД retention", "error", err)
		return exitFatal
	}
	defer pool.Close()
	var version int64
	if err = pool.QueryRow(ctx, `SELECT version_id FROM schema_migrations WHERE is_applied ORDER BY id DESC LIMIT 1`).Scan(&version); err != nil || version != requiredSchemaVersion {
		slog.Error("Retention потребує актуальної схеми; спочатку оновіть parser", "schema_version", version)
		return exitFatal
	}
	summary, err := applyDataRetention(ctx, pool, opts)
	if err != nil {
		slog.Error("Retention не завершено; незавершена транзакція відкочена", "error", err)
		return exitFatal
	}
	if err = json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return exitFatal
	}
	return exitSuccess
}

func applyDataRetention(ctx context.Context, pool *pgxpool.Pool, opts retentionOptions) (retentionSummary, error) {
	summary := retentionSummary{DryRun: !opts.Apply}
	if err := validateRetentionOptions(opts); err != nil {
		return summary, err
	}
	var cutoff time.Time
	if err := pool.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP-($1::INT*INTERVAL '1 day')`, opts.KeepDays).Scan(&cutoff); err != nil {
		return summary, err
	}
	if opts.ForgetSchedule != "" {
		if err := forgetDisabledSchedule(ctx, pool, opts); err != nil {
			return summary, err
		}
		if opts.Apply {
			summary.ForgottenSchedules = 1
		}
		return summary, nil
	}
	if !opts.Apply {
		rows, err := pool.Query(ctx, retentionCandidateSQL, cutoff, opts.MaxRuns)
		if err != nil {
			return summary, err
		}
		defer rows.Close()
		for rows.Next() {
			summary.EligibleRuns++
		}
		return summary, rows.Err()
	}
	for summary.DeletedRuns < opts.MaxRuns {
		deleted, err := deleteExpiredAuditRun(ctx, pool, cutoff)
		if err != nil {
			return summary, err
		}
		if !deleted {
			break
		}
		summary.DeletedRuns++
	}
	summary.EligibleRuns = summary.DeletedRuns
	// Не стираємо джерела failed/abandoned запусків: вони потрібні для resume.
	for batch := 0; batch < 1000; batch++ {
		tag, err := pool.Exec(ctx, `WITH batch AS (
		 SELECT t.run_id,t.target_id FROM audit_run_targets t JOIN audit_runs r ON r.id=t.run_id
		 WHERE r.status IN ('completed','completed_with_errors','canceled') AND t.request_url<>''
		 AND t.status IN ('completed','failed','canceled')
		 AND NOT EXISTS(SELECT 1 FROM audit_schedule_runs s WHERE s.run_id=r.id AND s.comparison_status='pending')
		 ORDER BY t.run_id,t.target_id LIMIT 100 FOR UPDATE OF t SKIP LOCKED)
		 UPDATE audit_run_targets t SET request_url='',request_url_cleared_at=CURRENT_TIMESTAMP
		 FROM batch b WHERE t.run_id=b.run_id AND t.target_id=b.target_id`)
		if err != nil {
			return summary, err
		}
		n := int(tag.RowsAffected())
		summary.ClearedTargetURLs += n
		if n == 0 {
			break
		}
	}
	return summary, nil
}

func deleteExpiredAuditRun(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollbackSiteTransaction(tx, Config{})
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(8247310022)`).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("інший retention уже працює")
	}
	var id string
	if err = tx.QueryRow(ctx, retentionCandidateSQL+` FOR UPDATE OF r SKIP LOCKED`, cutoff, 1).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM audit_runs WHERE id=$1`, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func forgetDisabledSchedule(ctx context.Context, pool *pgxpool.Pool, opts retentionOptions) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackSiteTransaction(tx, Config{})
	var enabled, active bool
	if err = tx.QueryRow(ctx, `SELECT enabled FROM audit_schedules WHERE id=$1 FOR UPDATE`, opts.ForgetSchedule).Scan(&enabled); err != nil {
		return err
	}
	if enabled {
		return fmt.Errorf("спочатку вимкніть розклад")
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_schedule_runs s JOIN audit_runs r ON r.id=s.run_id
	 WHERE s.schedule_id=$1 AND (r.status='running' OR s.comparison_status='pending'))
	 OR EXISTS(SELECT 1 FROM geo_profile_runs j JOIN geo_profiles p ON p.id=j.profile_id
	 WHERE p.schedule_id=$1 AND j.status='pending')`, opts.ForgetSchedule).Scan(&active); err != nil {
		return err
	}
	if active {
		return fmt.Errorf("розклад має незавершену роботу; дочекайтеся її завершення")
	}
	if !opts.Apply {
		return nil
	}
	for _, query := range []string{
		`DELETE FROM geo_profile_runs WHERE profile_id IN (SELECT id FROM geo_profiles WHERE schedule_id=$1)`,
		`DELETE FROM geo_profiles WHERE schedule_id=$1`,
		`DELETE FROM audit_schedules WHERE id=$1`,
	} {
		if _, err = tx.Exec(ctx, query, opts.ForgetSchedule); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
