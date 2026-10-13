package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/performance"
)

type cruxObservation struct {
	FetchedAt time.Time               `json:"fetched_at"`
	Report    *performance.CrUXReport `json:"report"`
}

func (s *webServer) cruxHistory(ctx context.Context, id string, target int64, device, scope string) ([]cruxObservation, error) {
	rows, err := s.pool.Query(ctx, `SELECT fetched_at,report FROM audit_crux_observations WHERE run_id=$1 AND target_id=$2 AND device=$3 AND scope=$4 ORDER BY fetched_at DESC,id DESC LIMIT 14`, id, target, device, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []cruxObservation{}
	for rows.Next() {
		var item cruxObservation
		if err = rows.Scan(&item.FetchedAt, &item.Report); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *webServer) crux(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	target, err := strconv.ParseInt(r.PathValue("target"), 10, 64)
	if err != nil || target < 1 {
		writeWebError(w, 400, "invalid_target", "Некоректний target ID")
		return
	}
	device, scope := r.URL.Query().Get("device"), r.URL.Query().Get("scope")
	if device == "" {
		device = "PHONE"
	}
	if scope == "" {
		scope = "url"
	}
	if (device != "PHONE" && device != "DESKTOP") || (scope != "url" && scope != "origin") {
		writeWebError(w, 400, "invalid_scope", "Потрібні PHONE/DESKTOP та url/origin")
		return
	}
	run, err := loadWebRun(r.Context(), s.pool, s.cfg, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	var safe string
	err = s.pool.QueryRow(r.Context(), `SELECT safe_url FROM audit_results WHERE run_id=$1 AND target_id=$2`, id, target).Scan(&safe)
	if err != nil {
		webDBError(w, err)
		return
	}
	history, err := s.cruxHistory(r.Context(), id, target, device, scope)
	if err != nil {
		webDBError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		writeWebJSON(w, 200, map[string]any{"configured": s.cfg.CrUXAPIKey != "", "history": history})
		return
	}
	var input struct {
		Confirm bool `json:"confirm_public_url"`
		Refresh bool `json:"refresh"`
	}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	if !input.Confirm {
		writeWebError(w, 400, "confirmation_required", "Підтвердьте передавання публічної адреси Google")
		return
	}
	if s.cfg.CrUXAPIKey == "" {
		writeWebError(w, 409, "crux_unconfigured", "CrUX не налаштовано")
		return
	}
	if run.Status == auditRunStatusRunning {
		writeWebError(w, 409, "audit_running", "Дочекайтеся завершення аудиту")
		return
	}
	if !input.Refresh && len(history) > 0 && time.Since(history[0].FetchedAt) < 24*time.Hour {
		writeWebJSON(w, 200, map[string]any{"configured": true, "history": history, "cached": true})
		return
	}
	u, err := url.Parse(safe)
	if err != nil {
		writeWebError(w, 400, "invalid_url", "Некоректна адреса")
		return
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	address, err := normalizeTargetURL(u.String(), false)
	if err != nil {
		writeWebError(w, 400, "private_url", "CrUX доступний лише для публічних адрес")
		return
	}
	if scope == "origin" {
		normalized, _ := url.Parse(address)
		address = normalized.Scheme + "://" + normalized.Host
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 429, "busy", "Інша операція виконується")
		return
	}
	now := time.Now().UnixNano()
	old := s.cruxNext.Load()
	if old > now || !s.cruxNext.CompareAndSwap(old, now+int64(3*time.Second)) {
		writeWebError(w, 429, "rate_limited", "Зачекайте перед наступним CrUX-запитом")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var report *performance.CrUXReport
	if s.cruxQuery != nil {
		report, err = s.cruxQuery(ctx, s.cfg.CrUXAPIKey, address, device, scope)
	} else {
		cfg := s.cfg
		cfg.AllowPrivateTargets = false
		transport := newHTTPTransport(cfg, 5*time.Second)
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		report, err = performance.QueryCrUX(ctx, client, s.cfg.CrUXAPIKey, address, device, scope)
	}
	if err != nil {
		if len(history) > 0 {
			writeWebJSON(w, 200, map[string]any{"configured": true, "history": history, "refresh_failed": true})
			return
		}
		writeWebError(w, 502, "provider_failed", "Не вдалося отримати CrUX; це не означає погані CWV")
		return
	}
	raw, e := json.Marshal(report)
	if e != nil || report == nil || len(raw) > 16384 {
		writeWebError(w, 502, "invalid_response", "Відповідь CrUX не збережено")
		return
	}
	// Upsert ідемпотентний у межах дня; невизначений COMMIT не повторюємо сліпо.
	_, err = s.pool.Exec(r.Context(), `INSERT INTO audit_crux_observations(run_id,target_id,device,scope,report) VALUES($1,$2,$3,$4,$5) ON CONFLICT(run_id,target_id,device,scope,observed_on) DO UPDATE SET report=EXCLUDED.report,fetched_at=CURRENT_TIMESTAMP`, id, target, device, scope, raw)
	if err != nil {
		webDBError(w, err)
		return
	}
	history, err = s.cruxHistory(r.Context(), id, target, device, scope)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"configured": true, "history": history, "cached": false})
}
