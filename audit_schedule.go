package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const schedulePollInterval = 15 * time.Second

var errScheduleConflict = errors.New("розклад із цим ID вже існує або досягнуто ліміт 100 розкладів")

type scheduleSource struct {
	URLs             []string          `json:"urls,omitempty"`
	Site             *siteCrawlOptions `json:"site,omitempty"`
	RenderJavaScript bool              `json:"render_javascript"`
}

type scheduleInput struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	URLs             string            `json:"urls"`
	Mode             string            `json:"mode"`
	Site             *siteCrawlOptions `json:"site"`
	RenderJavaScript bool              `json:"render_javascript"`
	IntervalHours    int               `json:"interval_hours"`
	NextRunAt        time.Time         `json:"next_run_at"`
}

func validateScheduleTiming(hours int, at time.Time, now time.Time) error {
	if hours < 1 || hours > 720 || at.IsZero() || at.Before(now.Add(-time.Minute)) || at.After(now.AddDate(1, 0, 0)) {
		return fmt.Errorf("інтервал: 1–720 годин; перший запуск: від поточного часу до року вперед")
	}
	return nil
}

func prepareSchedule(input *scheduleInput, cfg Config, maxURLs int) (scheduleSource, string, int, error) {
	var source scheduleSource
	input.Name = strings.TrimSpace(input.Name)
	if !runIDPattern.MatchString(input.ID) || !utf8.ValidString(input.Name) || strings.ContainsRune(input.Name, 0) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 120 {
		return source, "", 0, fmt.Errorf("потрібні коректний ID і назва до 120 символів")
	}
	if err := validateScheduleTiming(input.IntervalHours, input.NextRunAt, time.Now()); err != nil {
		return source, "", 0, err
	}
	cfg.RenderJavaScript = input.RenderJavaScript
	if err := validateRendering(cfg); err != nil {
		return source, "", 0, err
	}
	source.RenderJavaScript = input.RenderJavaScript
	if input.Mode == "site" && input.Site != nil && strings.TrimSpace(input.URLs) == "" {
		if err := validateSiteOptions(input.Site, cfg.AllowPrivateTargets); err != nil {
			return source, "", 0, err
		}
		source.Site = input.Site
		return source, redactURL(input.Site.RootURL), input.Site.MaxPages, nil
	}
	if (input.Mode != "" && input.Mode != "list") || input.Site != nil {
		return source, "", 0, fmt.Errorf("виберіть список URL або обхід сайту")
	}
	urls, err := parseWebURLs(input.URLs, maxURLs, cfg.AllowPrivateTargets)
	if err != nil {
		return source, "", 0, err
	}
	source.URLs = urls
	return source, redactURL(urls[0]), len(urls), nil
}

func nextScheduleTime(due, now time.Time, hours int) time.Time {
	if due.After(now) {
		return due
	}
	interval := time.Duration(hours) * time.Hour
	return due.Add((now.Sub(due)/interval + 1) * interval)
}

func (s *webServer) storeSchedule(ctx context.Context, input scheduleInput, source scheduleSource, label string, count int) error {
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	// Серіалізуємо лише створення розкладів і перевірку їхнього ліміту.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8247310018)`); err != nil {
		return err
	}
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT request_digest FROM audit_schedules WHERE id=$1`, input.ID).Scan(&existing)
	if err == nil {
		if string(existing) != string(digest[:]) {
			return errScheduleConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var total int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_schedules`).Scan(&total); err != nil {
		return err
	}
	if total >= 100 {
		return errScheduleConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_schedules(id,name,interval_hours,next_run_at,source,request_digest,source_label,target_count)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, input.ID, redactText(input.Name), input.IntervalHours, input.NextRunAt, encoded, digest[:], label, count)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (m *AuditManager) startScheduled(ctx context.Context, scheduleID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(m.cfg)+effectiveDBWriteTimeout(m.cfg))
	defer cancel()
	stop := context.AfterFunc(m.root, cancel)
	defer stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.root.Err() != nil || m.active != "" {
		return "", errAuditBusy
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollbackSiteTransaction(tx, m.cfg)
	var id string
	var due, now time.Time
	var hours int
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT s.id::TEXT,s.next_run_at,s.interval_hours,s.source,CURRENT_TIMESTAMP
		FROM audit_schedules s WHERE
		(($1='' AND s.enabled AND s.next_run_at<=CURRENT_TIMESTAMP) OR s.id::TEXT=$1)
		AND NOT EXISTS(SELECT 1 FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
		 WHERE sr.schedule_id=s.id AND (r.status='running' OR sr.comparison_status='pending'))
		ORDER BY s.next_run_at,s.id LIMIT 1 FOR UPDATE OF s SKIP LOCKED`, scheduleID).Scan(&id, &due, &hours, &raw, &now)
	if err != nil {
		return "", err
	}
	// Після row lock потрібен новий snapshot: попередній SELECT міг початися до COMMIT іншого менеджера.
	var inProgress bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
		WHERE sr.schedule_id=$1 AND (r.status='running' OR sr.comparison_status='pending'))`, id).Scan(&inProgress); err != nil {
		return "", err
	}
	if inProgress {
		return "", pgx.ErrNoRows
	}
	var source scheduleSource
	if err = json.Unmarshal(raw, &source); err != nil {
		return "", pauseInvalidSchedule(ctx, tx, id, err)
	}
	cfg := m.cfg
	cfg.RunID, cfg.OwnerGeneration, cfg.RenderJavaScript = newWebRunID(), 1, source.RenderJavaScript
	if err = validateRendering(cfg); err != nil {
		return "", pauseInvalidSchedule(ctx, tx, id, err)
	}
	if source.Site != nil {
		if err = validateSiteOptions(source.Site, cfg.AllowPrivateTargets); err != nil {
			return "", pauseInvalidSchedule(ctx, tx, id, err)
		}
		err = insertSiteAuditRun(ctx, tx, cfg, *source.Site)
	} else {
		// Перевіряємо поточну політику безпеки також після перезапуску сервісу.
		var urls []string
		urls, err = parseWebURLs(strings.Join(source.URLs, "\n"), 10000, cfg.AllowPrivateTargets)
		if err != nil {
			return "", pauseInvalidSchedule(ctx, tx, id, err)
		}
		err = insertExplicitAuditRun(ctx, tx, cfg, urls)
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_schedule_runs(run_id,schedule_id,key_check,scheduled_at) VALUES($1,$2,$3,$4)`,
		cfg.RunID, id, fingerprintURL(cfg.TargetFingerprintKey, "schedule:"+id), now)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE audit_schedules SET next_run_at=$2 WHERE id=$1`, id, nextScheduleTime(due, now, hours))
	if err != nil {
		return "", err
	}
	// Запуск, snapshot і наступний час фіксуються разом. Невизначений COMMIT не повторюємо.
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	m.launchLocked(cfg)
	return cfg.RunID, nil
}

func pauseInvalidSchedule(ctx context.Context, tx pgx.Tx, id string, cause error) error {
	message, _, _ := limitStorageString("Розклад призупинено: "+redactText(cause.Error()), 1000)
	if _, err := tx.Exec(ctx, `UPDATE audit_schedules SET enabled=FALSE,last_error=$2 WHERE id=$1`, id, message); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return fmt.Errorf("%s", message)
}

func (s *webServer) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulePollInterval)
	defer ticker.Stop()
	var lastRecovery time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastRecovery) >= time.Minute {
			lastRecovery = time.Now()
			var stale bool
			checkCtx, stop := context.WithTimeout(ctx, effectiveDBFetchTimeout(s.cfg))
			err := s.pool.QueryRow(checkCtx, `SELECT EXISTS(SELECT 1 FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
			 WHERE r.status='running' AND r.heartbeat_at<CURRENT_TIMESTAMP-$1::INTERVAL)`, effectiveStaleRunThreshold(s.cfg).String()).Scan(&stale)
			stop()
			if err == nil && stale {
				recoveryCtx, cancel := context.WithTimeout(ctx, s.cfg.FinalizationTimeout)
				_, err = abandonStaleAuditRuns(recoveryCtx, s.pool, s.cfg)
				cancel()
			}
			if err != nil && ctx.Err() == nil {
				slog.Warn("Не вдалося перевірити перервані планові аудити", "error", sanitizeError(err))
			}
		}
		for range 10 {
			workCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			done, err := s.compareScheduledRun(workCtx)
			stop()
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("Не вдалося порівняти планові аудити", "error", sanitizeError(err))
				}
				break
			}
			if !done {
				break
			}
		}
		if _, err := s.manager.startScheduled(ctx, ""); err != nil && !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, errAuditBusy) && ctx.Err() == nil {
			slog.Warn("Не вдалося запустити плановий аудит", "error", sanitizeError(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
