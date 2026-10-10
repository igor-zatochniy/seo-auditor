package main

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type observedVisibilityChange struct {
	Query  string `json:"query"`
	URL    string `json:"url"`
	Scope  string `json:"scope"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
}
type identifiedObservation struct {
	Record      geo.VisibilityRecord
	Fingerprint []byte
	KeyID       string
}

// URL після redaction не є ідентифікатором. За ротації HMAC-ключа порівняння URL пропускається.
func observationIdentity(v identifiedObservation) string {
	r := v.Record
	if r.Scope == "query" && r.ReportOrdinal != nil {
		return "query\x00" + geo.QueryKey(r.Query)
	}
	if len(v.Fingerprint) != 32 || v.KeyID == "" || r.TargetID == nil {
		return ""
	}
	identity := r.Scope + "\x00" + v.KeyID + "\x00" + hex.EncodeToString(v.Fingerprint)
	if r.Scope == "query_url" {
		if r.ReportOrdinal == nil {
			return ""
		}
		identity += "\x00" + geo.QueryKey(r.Query)
	}
	return identity
}

func compareVisibilityObservations(previous, current []identifiedObservation) []observedVisibilityChange {
	old := map[string]identifiedObservation{}
	for _, v := range previous {
		if key := observationIdentity(v); key != "" {
			old[key] = v
		}
	}
	out := []observedVisibilityChange{}
	for _, v := range current {
		key := observationIdentity(v)
		p, ok := old[key]
		if key == "" || !ok || p.Record.Impressions == nil || v.Record.Impressions == nil {
			continue
		}
		a, b := *p.Record.Impressions, *v.Record.Impressions
		if (a == 0 && b > 0) || (a > 0 && b == 0) {
			out = append(out, observedVisibilityChange{v.Record.Query, v.Record.URL, v.Record.Scope, a, b})
		}
	}
	return out
}

func loadIdentifiedObservations(ctx context.Context, tx pgx.Tx, importID string) ([]identifiedObservation, error) {
	rows, err := tx.Query(ctx, `SELECT v.observation,a.target_fingerprint,COALESCE(a.fingerprint_key_id,'')
	 FROM geo_visibility_rows v JOIN geo_visibility_imports i ON i.id=v.import_id JOIN geo_reports g ON g.id=i.report_id
	 LEFT JOIN audit_results a ON a.run_id=g.source_run_id AND a.target_id=(v.observation->>'target_id')::bigint
	 WHERE v.import_id=$1 ORDER BY v.ordinal LIMIT 5000`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []identifiedObservation{}
	for rows.Next() {
		var v identifiedObservation
		if err = rows.Scan(&v.Record, &v.Fingerprint, &v.KeyID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *webServer) geoProfileObservationChanges(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if r.URL.Query().Get("after") == "" {
		after, err = 0, nil
	}
	if !runIDPattern.MatchString(runID) || err != nil || after < 0 || after > 5000 {
		writeWebError(w, 400, "invalid_cursor", "Оберіть запуск і коректну сторінку")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var current string
	var previous *string
	if err = tx.QueryRow(ctx, "SELECT report_id::text,baseline_report_id::text FROM geo_profile_runs WHERE profile_id=$1 AND run_id=$2 AND status='ready'", id, runID).Scan(&current, &previous); err != nil {
		webDBError(w, err)
		return
	}
	response := map[string]any{"items": []observedVisibilityChange{}, "next": 0, "current": nil, "previous": nil}
	if previous == nil {
		writeWebJSON(w, 200, response)
		return
	}
	now, err := scanVisibilityImport(tx.QueryRow(ctx, "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE report_id=$1 AND source IN ('gsc_ai','ai_observed') ORDER BY created_at DESC,id DESC LIMIT 1", current))
	if errors.Is(err, pgx.ErrNoRows) {
		writeWebJSON(w, 200, response)
		return
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	before, err := scanVisibilityImport(tx.QueryRow(ctx, "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE report_id=$1 AND source=$2 AND engine=$3 AND country=$4 AND device=$5 AND period_end<$6::date ORDER BY period_end DESC,created_at DESC,id DESC LIMIT 1", *previous, now.Source, now.Engine, now.Country, now.Device, now.PeriodStart))
	if errors.Is(err, pgx.ErrNoRows) {
		writeWebJSON(w, 200, response)
		return
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	a, err := loadIdentifiedObservations(ctx, tx, before.ID)
	if err != nil {
		webDBError(w, err)
		return
	}
	b, err := loadIdentifiedObservations(ctx, tx, now.ID)
	if err != nil {
		webDBError(w, err)
		return
	}
	items := compareVisibilityObservations(a, b)
	end := min(after+50, len(items))
	start := min(after, end)
	response["items"], response["current"], response["previous"], response["total"] = items[start:end], now, before, len(items)
	if end < len(items) {
		response["next"] = end
	}
	writeWebJSON(w, 200, response)
}
