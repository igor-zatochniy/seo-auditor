package main

import (
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *webServer) registerAuditRules(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/schedules/{id}/rules", s.auditRuleList)
	mux.HandleFunc("POST /api/schedules/{id}/rules", s.auditRuleSave)
	mux.HandleFunc("GET /api/schedules/{id}/rules/{rule}/exceptions", s.auditRuleExceptions)
	mux.HandleFunc("POST /api/schedules/{id}/rules/{rule}/exceptions", s.auditRuleExceptionSave)
	mux.HandleFunc("DELETE /api/schedules/{id}/rules/{rule}/exceptions/{fingerprint}", s.auditRuleExceptionDelete)
	mux.HandleFunc("GET /api/audits/{id}/rules", s.auditRuleReport)
	mux.HandleFunc("POST /api/audits/{id}/rules/apply", s.auditRuleApply)
}

func (s *webServer) auditRuleExceptions(w http.ResponseWriter, r *http.Request) {
	schedule, ok := webRunID(w, r)
	if !ok {
		return
	}
	ruleID := r.PathValue("rule")
	if !runIDPattern.MatchString(ruleID) {
		writeWebError(w, 400, "invalid_rule", "Некоректне правило")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('fingerprint',encode(fingerprint,'hex'),'safe_url',safe_url,'reason',reason,'expires_at',expires_at) FROM audit_rule_exceptions WHERE schedule_id=$1 AND rule_id=$2 AND expires_at>CURRENT_TIMESTAMP ORDER BY expires_at LIMIT 500`, schedule, ruleID)
	if err != nil {
		webDBError(w, err)
		return
	}
	items, err := diagnosticRows(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, items)
}

func (s *webServer) auditRuleExceptionSave(w http.ResponseWriter, r *http.Request) {
	schedule, ok := webRunID(w, r)
	if !ok {
		return
	}
	ruleID := r.PathValue("rule")
	if !runIDPattern.MatchString(ruleID) {
		writeWebError(w, 400, "invalid_rule", "Некоректне правило")
		return
	}
	var input struct {
		URL          string `json:"url"`
		Reason       string `json:"reason"`
		ExpiresHours int    `json:"expires_hours"`
	}
	if !decodeWebJSON(w, r, 4096, &input) {
		return
	}
	normalized, normalizeErr := normalizeTargetURL(strings.TrimSpace(input.URL), s.cfg.AllowPrivateTargets)
	if normalizeErr != nil || !utf8.ValidString(input.Reason) || strings.TrimSpace(input.Reason) == "" || utf8.RuneCountInString(input.Reason) > 250 || input.ExpiresHours < 1 || input.ExpiresHours > 8760 {
		writeWebError(w, 400, "invalid_exception", "Вкажіть URL, причину до 250 символів і строк 1–8760 годин")
		return
	}
	input.URL = normalized
	var exists bool
	fingerprint := fingerprintURL(s.cfg.TargetFingerprintKey, input.URL)
	err := s.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM audit_custom_rules c WHERE c.id=$2 AND c.schedule_id=$1) AND EXISTS(
 SELECT 1 FROM audit_schedule_runs sr JOIN audit_results ar ON ar.run_id=sr.run_id WHERE sr.schedule_id=$1 AND ar.target_fingerprint=$3 AND ar.fingerprint_key_id=$4)`, schedule, ruleID, fingerprint, s.cfg.TargetFingerprintKeyID).Scan(&exists)
	if err != nil {
		webDBError(w, err)
		return
	}
	if !exists {
		writeWebError(w, 404, "target_not_found", "URL має бути присутній у попередньому аудиті цього розкладу")
		return
	}
	var rule customAuditRule
	if err = s.pool.QueryRow(r.Context(), `SELECT definition FROM audit_custom_rules WHERE id=$1 AND schedule_id=$2`, ruleID, schedule).Scan(&rule); err != nil {
		webDBError(w, err)
		return
	}
	parsedURL, err := url.Parse(input.URL)
	if err != nil || !auditRulePatternMatches(rule.Pattern, parsedURL) {
		writeWebError(w, 400, "exception_out_of_scope", "URL не відповідає області цього правила")
		return
	}
	reason := redactText(strings.TrimSpace(input.Reason))
	reason, _, _ = limitStorageString(reason, 500)
	expires := time.Now().Add(time.Duration(input.ExpiresHours) * time.Hour)
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(8247310040)`); err != nil {
		webDBError(w, err)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_rule_exceptions WHERE schedule_id=$1 AND expires_at>CURRENT_TIMESTAMP AND NOT (rule_id=$2 AND fingerprint=$3)`, schedule, ruleID, fingerprint).Scan(&count); err != nil {
		webDBError(w, err)
		return
	}
	if count >= 500 {
		writeWebError(w, 409, "exception_limit", "Для одного розкладу дозволено не більше 500 активних виключень")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_rule_exceptions(schedule_id,rule_id,fingerprint,safe_url,reason,expires_at) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(schedule_id,rule_id,fingerprint) DO UPDATE SET safe_url=EXCLUDED.safe_url,reason=EXCLUDED.reason,expires_at=EXCLUDED.expires_at,created_at=CURRENT_TIMESTAMP`, schedule, ruleID, fingerprint, redactURL(input.URL), reason, expires)
	if err != nil {
		webDBError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"ok": true, "fingerprint": hex.EncodeToString(fingerprint), "expires_at": expires})
}

func (s *webServer) auditRuleExceptionDelete(w http.ResponseWriter, r *http.Request) {
	schedule, ok := webRunID(w, r)
	if !ok {
		return
	}
	ruleID := r.PathValue("rule")
	raw := r.PathValue("fingerprint")
	if !runIDPattern.MatchString(ruleID) || len(raw) != 64 {
		writeWebError(w, 400, "invalid_exception", "Некоректне виключення")
		return
	}
	fp, err := hex.DecodeString(raw)
	if err != nil {
		writeWebError(w, 400, "invalid_exception", "Некоректний fingerprint")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM audit_rule_exceptions WHERE schedule_id=$1 AND rule_id=$2 AND fingerprint=$3`, schedule, ruleID, fp)
	if err != nil {
		webDBError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeWebError(w, 404, "exception_not_found", "Виключення не знайдено")
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
func (s *webServer) auditRuleList(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	rows, e := s.pool.Query(r.Context(), `SELECT definition || jsonb_build_object('last_notified_at',last_notified_at) FROM audit_custom_rules WHERE schedule_id=$1 ORDER BY id LIMIT 32`, id)
	if e != nil {
		webDBError(w, e)
		return
	}
	items, e := diagnosticRows(rows)
	if e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 200, items)
}
func (s *webServer) auditRuleSave(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var rule customAuditRule
	if !decodeWebJSON(w, r, 4096, &rule) {
		return
	}
	rule.Name = redactText(strings.TrimSpace(rule.Name))
	if e := validateAuditRule(rule); e != nil {
		writeWebError(w, 400, "invalid_rule", e.Error())
		return
	}
	tx, e := s.pool.Begin(r.Context())
	if e != nil {
		webDBError(w, e)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(8247310039)`); e != nil {
		webDBError(w, e)
		return
	}
	var schedule string
	if e = tx.QueryRow(r.Context(), `SELECT id::TEXT FROM audit_schedules WHERE id=$1`, id).Scan(&schedule); e != nil {
		webDBError(w, e)
		return
	}
	var old customAuditRule
	var oldSchedule string
	expectedRevision := rule.Revision
	e = tx.QueryRow(r.Context(), `SELECT definition,schedule_id::TEXT FROM audit_custom_rules WHERE id=$1`, rule.ID).Scan(&old, &oldSchedule)
	rule.Revision = 1
	if e == nil {
		if oldSchedule != id {
			writeWebError(w, 409, "rule_conflict", "ID належить іншому розкладу")
			return
		}
		rule.Revision = old.Revision
		if !equalRuleDefinition(old, rule) && expectedRevision != old.Revision {
			writeWebError(w, 409, "revision_conflict", "Правило змінено в іншому вікні; оновіть список")
			return
		}
		if !equalRuleDefinition(old, rule) {
			rule.Revision++
		}
	} else if errors.Is(e, pgx.ErrNoRows) {
		var count int
		if e = tx.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_custom_rules WHERE schedule_id=$1`, id).Scan(&count); e != nil {
			webDBError(w, e)
			return
		}
		if count >= 32 {
			writeWebError(w, 409, "rule_limit", "Не більше 32 правил на розклад")
			return
		}
	} else {
		webDBError(w, e)
		return
	}
	if _, e = tx.Exec(r.Context(), `INSERT INTO audit_custom_rules(id,schedule_id,revision,definition,enabled) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(id) DO UPDATE SET revision=EXCLUDED.revision,definition=EXCLUDED.definition,enabled=EXCLUDED.enabled,updated_at=CURRENT_TIMESTAMP`, rule.ID, id, rule.Revision, rule, rule.Enabled); e != nil {
		webDBError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 200, rule)
}
func (s *webServer) auditRuleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	if _, e := loadWebRun(r.Context(), s.pool, s.cfg, id); e != nil {
		webDBError(w, e)
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		var e error
		after, e = strconv.ParseInt(value, 10, 64)
		if e != nil || after < 0 {
			writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
			return
		}
	}
	var job map[string]any
	e := s.pool.QueryRow(r.Context(), `SELECT jsonb_build_object('status',status,'rules',rules,'checked',checked,'automatic',automatic,'schedule_id',schedule_id,
 'failed',(SELECT COUNT(*) FROM audit_rule_results WHERE run_id=$1 AND state='fail'),'unknown',(SELECT COUNT(*) FROM audit_rule_results WHERE run_id=$1 AND state='unknown')) FROM audit_rule_runs WHERE run_id=$1`, id).Scan(&job)
	if errors.Is(e, pgx.ErrNoRows) {
		writeWebJSON(w, 200, map[string]any{"job": nil, "items": []any{}, "next": 0})
		return
	}
	if e != nil {
		webDBError(w, e)
		return
	}
	rows, e := s.pool.Query(r.Context(), `WITH pages AS(SELECT DISTINCT target_id FROM audit_rule_results WHERE run_id=$1 AND target_id>$2 ORDER BY target_id LIMIT 26)
 SELECT jsonb_build_object('target_id',p.target_id,'url',MIN(r.safe_url),'checks',jsonb_agg(jsonb_build_object('id',r.rule_id,'revision',r.revision,'state',r.state,'evidence',r.evidence) ORDER BY r.rule_id))
 FROM pages p JOIN audit_rule_results r ON r.run_id=$1 AND r.target_id=p.target_id GROUP BY p.target_id ORDER BY p.target_id`, id, after)
	if e != nil {
		webDBError(w, e)
		return
	}
	items, e := diagnosticRows(rows)
	if e != nil {
		webDBError(w, e)
		return
	}
	next := int64(0)
	if len(items) > 25 {
		next = int64(items[24]["target_id"].(float64))
		items = items[:25]
	}
	writeWebJSON(w, 200, map[string]any{"job": job, "items": items, "next": next})
}
func (s *webServer) auditRuleApply(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var input struct {
		Schedule string `json:"schedule_id"`
	}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	if !runIDPattern.MatchString(input.Schedule) {
		writeWebError(w, 400, "invalid_schedule", "Потрібен UUID розкладу")
		return
	}
	run, e := loadWebRun(r.Context(), s.pool, s.cfg, id)
	if e != nil {
		webDBError(w, e)
		return
	}
	if run.Status != auditRunStatusCompleted && run.Status != auditRunStatusCompletedWithErrors {
		writeWebError(w, 409, "audit_incomplete", "Потрібен завершений аудит")
		return
	}
	tx, e := s.pool.Begin(r.Context())
	if e != nil {
		webDBError(w, e)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if e = captureAuditRules(r.Context(), tx, s.cfg, id, input.Schedule, false); e != nil {
		webDBError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 202, map[string]bool{"ok": true})
}
