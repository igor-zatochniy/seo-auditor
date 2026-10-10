package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
	"github.com/jackc/pgx/v5"
)

func (s *webServer) internationalReport(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	if err != nil || after < 0 {
		writeWebError(w, 400, "invalid_cursor", "Некоректний cursor hreflang")
		return
	}
	if _, err = loadWebRun(r.Context(), s.pool, s.cfg, id); err != nil {
		webDBError(w, err)
		return
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `SELECT a.target_id,a.safe_url,a.status_code,encode(a.target_fingerprint,'hex'),a.international,sm.evidence
	FROM audit_results a LEFT JOIN audit_sitemap_hreflang sm ON sm.run_id=a.run_id AND sm.page_fingerprint=a.target_fingerprint
	WHERE a.run_id=$1 AND a.target_id>$2 ORDER BY a.target_id LIMIT 51`, id, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	pages := make([]international.Page, 0, 50)
	ids := []int64{}
	positions := map[int64]int{}
	more := false
	for rows.Next() {
		if len(pages) == 50 {
			more = true
			break
		}
		var p international.Page
		var h, m []byte
		if err = rows.Scan(&p.TargetID, &p.URL, &p.HTTP, &p.Fingerprint, &h, &m); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		if len(h) > 0 {
			if err = json.Unmarshal(h, &p.HTML); err != nil {
				rows.Close()
				webDBError(w, err)
				return
			}
		}
		if len(m) > 0 {
			if err = json.Unmarshal(m, &p.Sitemap); err != nil {
				rows.Close()
				webDBError(w, err)
				return
			}
		}
		p.URL = redactURL(p.URL)
		p.Observations = map[string]international.Observation{}
		positions[p.TargetID] = len(pages)
		ids = append(ids, p.TargetID)
		pages = append(pages, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err = tx.Query(r.Context(), `SELECT src.target_id,a.value->>'fingerprint',d.status_code,COALESCE(d.scan_status,''),
	CASE WHEN d.status_code<>200 OR d.scan_status<>'completed' THEN NULL
	WHEN EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(d.international->'links','[]')||COALESCE(ds.evidence->'links','[]')) back
	 WHERE back->>'fingerprint'=encode(src.target_fingerprint,'hex') AND (back->>'valid_url')::BOOLEAN AND (back->>'absolute')::BOOLEAN AND (back->>'language_valid')::BOOLEAN) THEN TRUE
	WHEN (d.international->>'complete')::BOOLEAN AND (ds.evidence IS NULL OR (ds.evidence->>'complete')::BOOLEAN) THEN FALSE
	WHEN d.international IS NULL AND (ds.evidence->>'complete')::BOOLEAN THEN FALSE ELSE NULL END,
	CASE WHEN COALESCE(d.international->>'canonical_fingerprint','')='' THEN NULL ELSE d.international->>'canonical_fingerprint'=encode(d.target_fingerprint,'hex') END,
	COALESCE(d.international->>'canonical_fingerprint','')
	FROM audit_results src LEFT JOIN audit_sitemap_hreflang sm ON sm.run_id=src.run_id AND sm.page_fingerprint=src.target_fingerprint
	CROSS JOIN LATERAL jsonb_array_elements(COALESCE(src.international->'links','[]')||COALESCE(sm.evidence->'links','[]')) a
	LEFT JOIN LATERAL (SELECT v.* FROM audit_results v WHERE v.run_id=src.run_id AND v.target_fingerprint=
	 CASE WHEN a.value->>'fingerprint' ~ '^[0-9a-f]{64}$' THEN decode(a.value->>'fingerprint','hex') END ORDER BY v.target_id LIMIT 1) d ON TRUE
	LEFT JOIN audit_sitemap_hreflang ds ON ds.run_id=d.run_id AND ds.page_fingerprint=d.target_fingerprint
	WHERE src.run_id=$1 AND src.target_id=ANY($2::BIGINT[]) AND COALESCE(a.value->>'fingerprint','')<>''`, id, ids)
	if err != nil {
		webDBError(w, err)
		return
	}
	for rows.Next() {
		var target int64
		var fp string
		var o international.Observation
		if err = rows.Scan(&target, &fp, &o.HTTP, &o.Status, &o.Reciprocal, &o.CanonicalSelf, &o.CanonicalFingerprint); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		pages[positions[target]].Observations[fp] = o
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		webDBError(w, err)
		return
	}
	for i := range pages {
		international.Diagnose(&pages[i])
	}
	next := int64(0)
	if more {
		next = pages[len(pages)-1].TargetID
	}
	writeWebJSON(w, 200, map[string]any{"pages": pages, "next": next, "more": more, "basis": "response_html", "run_id": id})
}
