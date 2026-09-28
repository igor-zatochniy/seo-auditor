package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func decodeWebJSON(w http.ResponseWriter, r *http.Request, limit int64, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(dest)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeWebError(w, 413, "body_too_large", "Перевищено розмір запиту")
		} else {
			writeWebError(w, 400, "invalid_json", "Некоректний JSON запиту")
		}
		return false
	}
	return true
}

func (s *webServer) ready(w http.ResponseWriter, r *http.Request) {
	if s.manager != nil {
		s.manager.mu.Lock()
		closed := s.manager.closed || s.manager.root.Err() != nil
		s.manager.mu.Unlock()
		if closed {
			writeWebError(w, 503, "stopping", "Сервіс завершується")
			return
		}
	}
	if s.pool == nil {
		writeWebError(w, 503, "not_ready", "PostgreSQL недоступний")
		return
	}
	if err := s.pool.Ping(r.Context()); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]string{"status": "ready"})
}
func (s *webServer) submit(w http.ResponseWriter, r *http.Request) {
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "submission_active", "Інший запит уже обробляється")
		return
	}
	var body struct {
		URLs string `json:"urls"`
	}
	if !decodeWebJSON(w, r, min(int64(s.web.MaxURLs)*4096+1024, 8<<20), &body) {
		return
	}
	urls, err := parseWebURLs(body.URLs, s.web.MaxURLs, s.cfg.AllowPrivateTargets)
	if err != nil {
		writeWebError(w, 400, "invalid_urls", err.Error())
		return
	}
	id, err := s.manager.start(r.Context(), urls, "")
	if err != nil {
		handleStartError(w, err)
		return
	}
	w.Header().Set("Location", "/audits/"+id)
	writeWebJSON(w, 202, map[string]string{"id": id})
}
func handleStartError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAuditBusy) {
		writeWebError(w, 409, "audit_active", "Інший аудит ще виконується або завершується")
		return
	}
	if errors.Is(err, errAuditNotResumable) {
		writeWebError(w, 409, "not_resumable", "Цей аудит не можна відновити")
		return
	}
	webDBError(w, err)
}
func (s *webServer) history(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		if _, err := decodeHistoryCursor(cursor); err != nil {
			writeWebError(w, 400, "invalid_cursor", "Некоректний cursor")
			return
		}
	}
	p, err := loadRunHistory(r.Context(), s.pool, s.cfg, cursor)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, p)
}
func (s *webServer) runInfo(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	run, err := loadWebRun(r.Context(), s.pool, s.cfg, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, run)
}
func (s *webServer) progress(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	p, err := loadWebProgress(r.Context(), s.pool, s.cfg, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, p)
}
func (s *webServer) analytics(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	if _, err := loadWebRun(r.Context(), s.pool, s.cfg, id); err != nil {
		webDBError(w, err)
		return
	}
	a, err := loadWebAnalytics(r.Context(), s.pool, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, a)
}
func (s *webServer) results(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	q, err := parseResultQuery(r.URL.Query())
	if err != nil {
		writeWebError(w, 400, "invalid_query", err.Error())
		return
	}
	if _, err := loadWebRun(r.Context(), s.pool, s.cfg, id); err != nil {
		webDBError(w, err)
		return
	}
	p, err := loadResultPage(r.Context(), s.pool, id, q)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, p)
}
func (s *webServer) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	if !s.manager.cancelRun(id) {
		writeWebError(w, 409, "not_active", "Аудит не виконується в цьому процесі")
		return
	}
	writeWebJSON(w, 202, map[string]string{"status": "canceling"})
}
func (s *webServer) resume(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	id, err := s.manager.start(r.Context(), nil, id)
	if err != nil {
		handleStartError(w, err)
		return
	}
	writeWebJSON(w, 202, map[string]string{"id": id})
}
