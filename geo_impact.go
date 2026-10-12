package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

func loadImpactSamples(ctx context.Context, tx pgx.Tx, report geoReport) ([]geo.ImpactSample, error) {
	rows, err := loadEffectiveGEORows(ctx, tx, report.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.Result.TargetID != nil {
			ids = append(ids, *row.Result.TargetID)
		}
	}
	keys, err := tx.Query(ctx, "SELECT target_id,target_fingerprint,fingerprint_key_id FROM audit_results WHERE run_id=$1 AND target_id=ANY($2::bigint[])", report.SourceRunID, ids)
	if err != nil {
		return nil, err
	}
	type identity struct{ fingerprint, key string }
	identities := map[int64]identity{}
	for keys.Next() {
		var id int64
		var fingerprint []byte
		var key string
		if err = keys.Scan(&id, &fingerprint, &key); err != nil {
			keys.Close()
			return nil, err
		}
		identities[id] = identity{hex.EncodeToString(fingerprint), key}
	}
	err = keys.Err()
	keys.Close()
	if err != nil {
		return nil, err
	}
	out := []geo.ImpactSample{}
	for _, r := range rows {
		v := geo.ImpactSample{Result: r.Result, Mode: r.MappingMode}
		if r.Result.TargetID != nil {
			identity := identities[*r.Result.TargetID]
			v.Fingerprint, v.KeyID = identity.fingerprint, identity.key
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *webServer) geoImpactGet(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	baseline := r.URL.Query().Get("baseline")
	if !runIDPattern.MatchString(baseline) || baseline == id {
		writeWebError(w, 400, "invalid_baseline", "Оберіть інший попередній GEO-звіт.")
		return
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	a, err := scanGEOReport(tx.QueryRow(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", baseline))
	if err != nil {
		webDBError(w, err)
		return
	}
	b, err := scanGEOReport(tx.QueryRow(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	if a.Domain != b.Domain || a.CreatedAt.After(b.CreatedAt) {
		writeWebError(w, 400, "invalid_baseline", "Потрібен попередній звіт того самого домену.")
		return
	}
	before, err := loadImpactSamples(r.Context(), tx, a)
	if err != nil {
		webDBError(w, err)
		return
	}
	after, err := loadImpactSamples(r.Context(), tx, b)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"baseline": a, "current": b, "same_model": a.Model == b.Model, "items": geo.CompareImpact(a.Model, b.Model, before, after)})
}

type visibilityPoint struct {
	ImportID    string `json:"import_id"`
	ReportID    string `json:"report_id"`
	Start       string `json:"period_start"`
	End         string `json:"period_end"`
	Impressions *int64 `json:"impressions"`
	Clicks      *int64 `json:"clicks"`
	Rows        int    `json:"matched_rows"`
	Overlaps    bool   `json:"overlaps_previous"`
}

func (s *webServer) geoVisibilitySeries(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ordinal, err := strconv.Atoi(q.Get("ordinal"))
	source, engine, country, device, scope := q.Get("source"), q.Get("engine"), q.Get("country"), q.Get("device"), q.Get("scope")
	if err != nil || ordinal < 1 || ordinal > 1000 || (source != "gsc_ai" && source != "ai_observed") || (engine != "google_ai" && engine != "chatgpt" && engine != "gemini" && engine != "perplexity" && engine != "other") || (country != "" && (len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z')) || (device != "all" && device != "desktop" && device != "mobile" && device != "tablet") || (scope != "query" && scope != "query_url" && scope != "url") {
		writeWebError(w, 400, "invalid_series", "Оберіть запит, AI-джерело, систему, країну, пристрій і рівень даних. GSC Web не є AI-видимістю.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var query, domain string
	if err = tx.QueryRow(ctx, "SELECT q.result->>'query',g.domain FROM geo_query_results q JOIN geo_reports g ON g.id=q.report_id WHERE q.report_id=$1 AND q.ordinal=$2", id, ordinal).Scan(&query, &domain); err != nil {
		webDBError(w, err)
		return
	}
	var selectedFingerprint []byte
	var selectedKey string
	if scope == "url" {
		err = tx.QueryRow(ctx, `SELECT a.target_fingerprint,a.fingerprint_key_id FROM geo_query_results q
 JOIN geo_reports g ON g.id=q.report_id
 LEFT JOIN LATERAL (SELECT target_id FROM geo_target_decisions WHERE report_id=q.report_id AND ordinal=q.ordinal ORDER BY sequence DESC LIMIT 1) d ON TRUE
 JOIN audit_results a ON a.run_id=g.source_run_id AND a.target_id=COALESCE(d.target_id,(q.result->>'target_id')::bigint)
 WHERE q.report_id=$1 AND q.ordinal=$2`, id, ordinal).Scan(&selectedFingerprint, &selectedKey)
		if err != nil && err != pgx.ErrNoRows {
			webDBError(w, err)
			return
		}
	}
	imports, err := tx.Query(ctx, `SELECT id::text,report_id::text,period_start::text,period_end::text FROM
 (SELECT DISTINCT ON(i.period_start,i.period_end) i.id,i.report_id,i.period_start,i.period_end,i.created_at FROM geo_visibility_imports i JOIN geo_reports g ON g.id=i.report_id WHERE g.domain=$1 AND i.source=$2 AND i.engine=$3 AND i.country=$4 AND i.device=$5
 AND EXISTS(SELECT 1 FROM geo_visibility_rows v WHERE v.import_id=i.id AND v.observation->>'scope'=$6)
 ORDER BY i.period_start,i.period_end,i.created_at DESC,i.id DESC) selected ORDER BY period_start DESC,period_end DESC LIMIT 50`, domain, source, engine, country, device, scope)
	if err != nil {
		webDBError(w, err)
		return
	}
	points := []visibilityPoint{}
	for imports.Next() {
		var p visibilityPoint
		if err = imports.Scan(&p.ImportID, &p.ReportID, &p.Start, &p.End); err != nil {
			imports.Close()
			webDBError(w, err)
			return
		}
		points = append(points, p)
	}
	err = imports.Err()
	imports.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	for i := range points {
		rows, err := tx.Query(ctx, `SELECT v.observation,a.target_fingerprint,COALESCE(a.fingerprint_key_id,'') FROM geo_visibility_rows v
 JOIN geo_visibility_imports i ON i.id=v.import_id JOIN geo_reports g ON g.id=i.report_id
 LEFT JOIN audit_results a ON a.run_id=g.source_run_id AND a.target_id=(v.observation->>'target_id')::bigint
 WHERE v.import_id=$1 AND v.observation->>'scope'=$2 ORDER BY v.ordinal LIMIT 5000`, points[i].ImportID, scope)
		if err != nil {
			webDBError(w, err)
			return
		}
		var impressions, clicks int64
		knownImpressions, knownClicks := true, true
		for rows.Next() {
			var v geo.VisibilityRecord
			var fingerprint []byte
			var key string
			if err = rows.Scan(&v, &fingerprint, &key); err != nil {
				rows.Close()
				webDBError(w, err)
				return
			}
			matches := v.ReportOrdinal != nil && geo.QueryKey(v.Query) == geo.QueryKey(query)
			if scope == "url" {
				matches = len(selectedFingerprint) == 32 && selectedKey != "" && selectedKey == key && bytes.Equal(selectedFingerprint, fingerprint)
			}
			if !matches {
				continue
			}
			points[i].Rows++
			if v.Impressions == nil || *v.Impressions < 0 || (knownImpressions && *v.Impressions > math.MaxInt64-impressions) {
				knownImpressions = false
			} else if knownImpressions {
				impressions += *v.Impressions
			}
			if v.Clicks == nil || *v.Clicks < 0 || (knownClicks && *v.Clicks > math.MaxInt64-clicks) {
				knownClicks = false
			} else if knownClicks {
				clicks += *v.Clicks
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			webDBError(w, err)
			return
		}
		if points[i].Rows > 0 && knownImpressions {
			points[i].Impressions = &impressions
		}
		if points[i].Rows > 0 && knownClicks {
			points[i].Clicks = &clicks
		}
	}
	for left, right := 0, len(points)-1; left < right; left, right = left+1, right-1 {
		points[left], points[right] = points[right], points[left]
	}
	latestEnd := ""
	for i := range points {
		points[i].Overlaps = points[i].Start <= latestEnd
		if points[i].End > latestEnd {
			latestEnd = points[i].End
		}
	}
	writeWebJSON(w, 200, map[string]any{"query": query, "source": source, "engine": engine, "country": country, "device": device, "scope": scope, "points": points})
}
