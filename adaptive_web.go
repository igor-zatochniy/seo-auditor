package main

import (
	"crypto/hmac"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
)

func (s *webServer) adaptiveTargets(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.Atoi(raw)
		if err != nil || after < 0 || after > 10000 {
			writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
			return
		}
	}
	var source scheduleSource
	if err := s.pool.QueryRow(r.Context(), `SELECT source FROM audit_schedules WHERE id=$1`, id).Scan(&source); err != nil {
		webDBError(w, err)
		return
	}
	if !source.Adaptive {
		writeWebError(w, 400, "not_adaptive", "Розклад не є адаптивним")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('index',source_index,'priority',priority,'next_check_at',next_check_at,'stable_checks',stable_checks,'consecutive_failures',consecutive_failures,'baseline_run_id',baseline_run_id) FROM audit_adaptive_targets WHERE schedule_id=$1 AND source_index>$2 ORDER BY source_index LIMIT 101`, id, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	next := 0
	for rows.Next() {
		var item map[string]any
		if err = rows.Scan(&item); err != nil {
			webDBError(w, err)
			return
		}
		index := int(item["index"].(float64))
		if index < 1 || index > len(source.URLs) {
			writeWebError(w, 503, "invalid_source", "Snapshot цілей недоступний")
			return
		}
		if len(items) == 100 {
			next = int(items[99]["index"].(float64))
			break
		}
		item["safe_url"] = redactURL(source.URLs[index-1])
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next})
}

func (s *webServer) adaptivePriority(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	index, err := strconv.Atoi(r.PathValue("target"))
	if err != nil || index < 1 || index > 10000 {
		writeWebError(w, 400, "invalid_target", "Некоректна ціль")
		return
	}
	var input struct {
		Priority int `json:"priority"`
	}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	if input.Priority < 1 || input.Priority > 3 {
		writeWebError(w, 400, "invalid_priority", "Пріоритет: 1–3")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `UPDATE audit_adaptive_targets SET priority=$3 WHERE schedule_id=$1 AND source_index=$2`, id, index, input.Priority)
	if err != nil {
		webDBError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		webDBError(w, pgx.ErrNoRows)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}

func (s *webServer) recrawlTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	targetID, err := strconv.ParseInt(r.PathValue("target"), 10, 64)
	if err != nil || targetID < 1 {
		writeWebError(w, 400, "invalid_target", "Некоректна ціль")
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if !decodeWebJSON(w, r, 4096, &input) {
		return
	}
	var safe, keyID string
	var fingerprint []byte
	var render bool
	err = s.pool.QueryRow(r.Context(), `SELECT safe_url,target_fingerprint,fingerprint_key_id,a.render_javascript FROM audit_results r JOIN audit_runs a ON a.id=r.run_id WHERE r.run_id=$1 AND r.target_id=$2`, id, targetID).Scan(&safe, &fingerprint, &keyID, &render)
	if err != nil {
		webDBError(w, err)
		return
	}
	if input.URL == "" {
		input.URL = safe
	}
	urls, err := parseWebURLs(input.URL, 1, s.cfg.AllowPrivateTargets)
	if err != nil {
		writeWebError(w, 400, "invalid_url", err.Error())
		return
	}
	currentKey := s.cfg.TargetFingerprintKeyID
	if currentKey == "" {
		currentKey = DefaultTargetFingerprintKeyID
	}
	if keyID != currentKey || !hmac.Equal(fingerprint, fingerprintURL(s.cfg.TargetFingerprintKey, urls[0])) {
		writeWebError(w, 409, "original_url_required", "Потрібен оригінальний URL; замаскований URL або змінений ключ не можна використати для повторної перевірки")
		return
	}
	runID, err := s.manager.startWithRendering(r.Context(), urls, "", render)
	if err != nil {
		handleStartError(w, err)
		return
	}
	writeWebJSON(w, 202, map[string]string{"id": runID})
}
