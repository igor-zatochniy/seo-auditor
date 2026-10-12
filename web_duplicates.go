package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/igor-zatochniy/seo-auditor/internal/duplicates"
	"github.com/jackc/pgx/v5"
)

type duplicateMember struct {
	TargetID  int64  `json:"target_id"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	H1        string `json:"h1"`
	Canonical string `json:"canonical"`
	Language  string `json:"language"`
	Words     int    `json:"words"`
}

type duplicateCluster struct {
	duplicates.Cluster
	Members    []duplicateMember `json:"members"`
	MemberNext int64             `json:"member_next"`
}

func duplicateCursor(r *http.Request, key string) (int64, error) {
	if r.URL.Query().Get(key) == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if n < 0 {
		return 0, strconv.ErrSyntax
	}
	return n, err
}

func (s *webServer) duplicateReport(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after, err := duplicateCursor(r, "after")
	clusterID, clusterErr := duplicateCursor(r, "cluster")
	memberAfter, memberErr := duplicateCursor(r, "member_after")
	if err != nil || clusterErr != nil || memberErr != nil {
		writeWebError(w, 400, "invalid_cursor", "Некоректний cursor груп дублів")
		return
	}
	if !s.duplicateBusy.CompareAndSwap(false, true) {
		writeWebError(w, 429, "busy", "Аналіз дублів уже виконується. Спробуйте ще раз")
		return
	}
	defer s.duplicateBusy.Store(false)
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var status string
	if err = tx.QueryRow(r.Context(), "SELECT status FROM audit_runs WHERE id=$1", id).Scan(&status); err != nil {
		webDBError(w, err)
		return
	}
	if status == auditRunStatusRunning || status == auditRunStatusAbandoned {
		writeWebError(w, 409, "audit_not_finished", "Дочекайтеся завершення або відновлення аудиту")
		return
	}
	detector := duplicates.NewDetector()
	cursor := int64(math.MinInt64)
	for {
		rows, queryErr := tx.Query(r.Context(), `SELECT a.target_id,a.content_signature,
		 COALESCE(a.international->>'canonical_fingerprint',''),a.fingerprint_key_id,
		 CASE WHEN length(a.title)<=500 AND NOT a.title_truncated THEN COALESCE(a.title,'') ELSE '' END,
		 CASE WHEN length(a.h1)<=1000 AND NOT a.h1_truncated THEN COALESCE(a.h1,'') ELSE '' END,
		 (COALESCE(a.geo_signals->'schema_types','[]') ? 'Product'),
		 EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(a.international->'links','[]') || COALESCE(sm.evidence->'links','[]')) e
		 WHERE e->>'language'<>'x-default' AND e->>'language_valid'='true' AND e->>'valid_url'='true'),
		 a.canonical_url_truncated OR (COALESCE(a.canonical_url,'')<>'' AND COALESCE(a.international->>'canonical_fingerprint','')='')
		 FROM audit_results a LEFT JOIN audit_sitemap_hreflang sm ON sm.run_id=a.run_id AND sm.page_fingerprint=a.target_fingerprint
		 WHERE a.run_id=$1 AND a.target_id>$2 AND a.status_code=200 AND a.scan_status='completed'
		 ORDER BY a.target_id LIMIT 256`, id, cursor)
		if queryErr != nil {
			webDBError(w, queryErr)
			return
		}
		count := 0
		for rows.Next() {
			var d duplicates.Document
			var raw []byte
			if err = rows.Scan(&d.TargetID, &raw, &d.Canonical, &d.KeyID, &d.TitleKey, &d.H1Key, &d.Product, &d.Hreflang, &d.CanonicalLost); err != nil {
				rows.Close()
				webDBError(w, err)
				return
			}
			if len(raw) > 0 {
				if err = json.Unmarshal(raw, &d.Signature); err != nil {
					rows.Close()
					webDBError(w, err)
					return
				}
			}
			d.TitleKey = duplicates.HeadingKey(d.TitleKey)
			d.H1Key = duplicates.HeadingKey(d.H1Key)
			detector.Add(d)
			cursor = d.TargetID
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			webDBError(w, err)
			return
		}
		if count < 256 {
			break
		}
	}
	summary, clusters, err := detector.Analyze(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	selected := []duplicateCluster{}
	next := int64(0)
	for _, c := range clusters {
		if clusterID != 0 && c.ID != clusterID || clusterID == 0 && c.ID <= after {
			continue
		}
		if len(selected) == 25 {
			next = selected[len(selected)-1].ID
			break
		}
		limit := 5
		start := int64(0)
		if clusterID != 0 {
			limit = 50
			start = memberAfter
		}
		ids := []int64{}
		more := false
		for _, target := range c.TargetIDs {
			if target <= start {
				continue
			}
			if len(ids) == limit {
				more = true
				break
			}
			ids = append(ids, target)
		}
		view := duplicateCluster{Cluster: c, Members: []duplicateMember{}}
		if len(ids) > 0 {
			rows, queryErr := tx.Query(r.Context(), `SELECT target_id,safe_url,LEFT(COALESCE(title,''),500),LEFT(COALESCE(h1,''),1000),
			 LEFT(COALESCE(canonical_url,''),2048),COALESCE(content_signature->>'language',''),(content_signature->>'words')::INTEGER
			 FROM audit_results WHERE run_id=$1 AND target_id=ANY($2::BIGINT[]) ORDER BY target_id`, id, ids)
			if queryErr != nil {
				webDBError(w, queryErr)
				return
			}
			for rows.Next() {
				var m duplicateMember
				if err = rows.Scan(&m.TargetID, &m.URL, &m.Title, &m.H1, &m.Canonical, &m.Language, &m.Words); err != nil {
					rows.Close()
					webDBError(w, err)
					return
				}
				m.URL = redactURL(m.URL)
				m.Canonical = redactURL(m.Canonical)
				m.Title = redactText(m.Title)
				m.H1 = redactText(m.H1)
				view.Members = append(view.Members, m)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				webDBError(w, err)
				return
			}
			if more {
				view.MemberNext = ids[len(ids)-1]
			}
		}
		selected = append(selected, view)
	}
	if clusterID != 0 && len(selected) == 0 {
		writeWebError(w, 404, "cluster_not_found", "Групу дублів не знайдено")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"run_id": id, "basis": "response_html", "summary": summary, "clusters": selected, "next": next})
}
