package main

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type webSchedule struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Enabled          bool      `json:"enabled"`
	IntervalHours    int       `json:"interval_hours"`
	NextRunAt        time.Time `json:"next_run_at"`
	SourceLabel      string    `json:"source_label"`
	TargetCount      int       `json:"target_count"`
	Mode             string    `json:"mode"`
	RenderJavaScript bool      `json:"render_javascript"`
	LastRunID        string    `json:"last_run_id"`
	LastStatus       string    `json:"last_status"`
	ComparisonStatus string    `json:"comparison_status"`
	ChangeCount      int       `json:"change_count"`
	LastError        string    `json:"last_error"`
}

func (s *webServer) registerSchedules(mux *http.ServeMux) {
	page := template.Must(template.ParseFS(webAssets, "web/templates/monitoring.html"))
	mux.HandleFunc("GET /monitoring", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.ExecuteTemplate(w, "monitoring.html", nil)
	})
	mux.HandleFunc("GET /api/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/schedules", s.createSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/update", s.updateSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/run", s.runSchedule)
	mux.HandleFunc("GET /api/schedules/{id}/runs", s.scheduleHistory)
	mux.HandleFunc("GET /api/changes", s.listChanges)
	mux.HandleFunc("POST /api/changes/read", s.readChanges)
}

func (s *webServer) createSchedule(w http.ResponseWriter, r *http.Request) {
	var input scheduleInput
	if !decodeWebJSON(w, r, min(int64(s.web.MaxURLs)*4096+4096, 8<<20), &input) {
		return
	}
	source, label, count, err := prepareSchedule(&input, s.cfg, s.web.MaxURLs)
	if err != nil {
		writeWebError(w, 400, "invalid_schedule", err.Error())
		return
	}
	if err = s.storeSchedule(r.Context(), input, source, label, count); err != nil {
		if errors.Is(err, errScheduleConflict) {
			writeWebError(w, 409, "schedule_conflict", err.Error())
			return
		}
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 201, map[string]string{"id": input.ID})
}

func (s *webServer) listSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT s.id::TEXT,s.name,s.enabled,s.interval_hours,s.next_run_at,s.source_label,s.target_count,
	 CASE WHEN s.source ? 'site' THEN 'site' ELSE 'list' END,COALESCE((s.source->>'render_javascript')::BOOLEAN,FALSE),
	 COALESCE(last.run_id::TEXT,''),COALESCE(last.status,''),COALESCE(last.comparison_status,''),COALESCE(last.change_count,0),s.last_error
	 FROM audit_schedules s LEFT JOIN LATERAL (
	 SELECT sr.run_id,r.status,sr.comparison_status,sr.change_count FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
	 WHERE sr.schedule_id=s.id ORDER BY sr.scheduled_at DESC,sr.run_id DESC LIMIT 1) last ON TRUE ORDER BY s.created_at DESC,s.id LIMIT 100`)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []webSchedule{}
	for rows.Next() {
		var item webSchedule
		if err = rows.Scan(&item.ID, &item.Name, &item.Enabled, &item.IntervalHours, &item.NextRunAt, &item.SourceLabel, &item.TargetCount, &item.Mode, &item.RenderJavaScript, &item.LastRunID, &item.LastStatus, &item.ComparisonStatus, &item.ChangeCount, &item.LastError); err != nil {
			webDBError(w, err)
			return
		}
		item.Name, item.SourceLabel, item.LastError = redactText(item.Name), redactURL(item.SourceLabel), redactText(item.LastError)
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, items)
}

func (s *webServer) updateSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var input struct {
		Name          string    `json:"name"`
		Enabled       bool      `json:"enabled"`
		IntervalHours int       `json:"interval_hours"`
		NextRunAt     time.Time `json:"next_run_at"`
	}
	if !decodeWebJSON(w, r, 2048, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !utf8.ValidString(input.Name) || strings.ContainsRune(input.Name, 0) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 120 {
		writeWebError(w, 400, "invalid_name", "Назва: 1–120 символів")
		return
	}
	if err := validateScheduleTiming(input.IntervalHours, input.NextRunAt, time.Now()); err != nil {
		writeWebError(w, 400, "invalid_schedule", err.Error())
		return
	}
	result, err := s.pool.Exec(r.Context(), `UPDATE audit_schedules SET name=$2,enabled=$3,interval_hours=$4,next_run_at=$5,last_error='' WHERE id=$1`, id, redactText(input.Name), input.Enabled, input.IntervalHours, input.NextRunAt)
	if err != nil {
		webDBError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		webDBError(w, pgx.ErrNoRows)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}

func (s *webServer) runSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	runID, err := s.manager.startScheduled(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeWebError(w, 409, "schedule_busy", "Розклад не знайдено або попередній аудит ще виконується чи порівнюється")
		return
	}
	if err != nil {
		handleStartError(w, err)
		return
	}
	writeWebJSON(w, 202, map[string]string{"id": runID})
}

func (s *webServer) scheduleHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('id',sr.run_id,'status',r.status,'started_at',r.started_at,
	 'baseline_run_id',sr.baseline_run_id,'comparison_status',sr.comparison_status,'change_count',sr.change_count)
	 FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id WHERE sr.schedule_id=$1 ORDER BY sr.scheduled_at DESC,sr.run_id DESC LIMIT 50`, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var item map[string]any
		if err = rows.Scan(&item); err != nil {
			webDBError(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, items)
}

func (s *webServer) listChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before := int64(9223372036854775807)
	if q.Get("before") != "" {
		var err error
		before, err = strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil || before < 1 {
			writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
			return
		}
	}
	runID := q.Get("run_id")
	if runID != "" && !runIDPattern.MatchString(runID) {
		writeWebError(w, 400, "invalid_run_id", "Некоректний ID")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('id',e.id,'run_id',e.run_id,'target_id',e.target_id,'safe_url',e.safe_url,
	 'kind',e.kind,'severity',e.severity,'before_value',e.before_value,'after_value',e.after_value,'created_at',e.created_at,'read',e.read_at IS NOT NULL,
	 'schedule_name',s.name,'baseline_run_id',sr.baseline_run_id)
	 FROM audit_change_events e JOIN audit_schedule_runs sr ON sr.run_id=e.run_id JOIN audit_schedules s ON s.id=sr.schedule_id
	 WHERE e.id<$1 AND ($2='' OR e.run_id::TEXT=$2) AND (NOT $3 OR e.read_at IS NULL) ORDER BY e.id DESC LIMIT 101`, before, runID, q.Get("unread") == "true")
	if err != nil {
		webDBError(w, err)
		return
	}
	items := []map[string]any{}
	for rows.Next() {
		var item map[string]any
		if err = rows.Scan(&item); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		for _, key := range []string{"safe_url", "before_value", "after_value", "schedule_name"} {
			if value, ok := item[key].(string); ok {
				item[key] = redactText(value)
			}
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	var unread int64
	if err = s.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_change_events WHERE read_at IS NULL`).Scan(&unread); err != nil {
		webDBError(w, err)
		return
	}
	next := ""
	if len(items) > 100 {
		next = strconv.FormatInt(int64(items[99]["id"].(float64)), 10)
		items = items[:100]
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next, "unread": unread})
}

func (s *webServer) readChanges(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeWebJSON(w, r, 4096, &body) {
		return
	}
	if len(body.IDs) < 1 || len(body.IDs) > 100 {
		writeWebError(w, 400, "invalid_ids", "Виберіть 1–100 сповіщень")
		return
	}
	_, err := s.pool.Exec(r.Context(), `UPDATE audit_change_events SET read_at=COALESCE(read_at,CURRENT_TIMESTAMP) WHERE id=ANY($1::BIGINT[])`, body.IDs)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
