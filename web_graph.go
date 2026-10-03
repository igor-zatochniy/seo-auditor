package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type siteGraphInfo struct {
	Root         string `json:"root"`
	MaxPages     int    `json:"max_pages"`
	MaxDepth     int    `json:"max_depth"`
	SitemapState string `json:"sitemap_state"`
	Warning      string `json:"warning"`
	Limited      bool   `json:"limited"`
	Ready        bool   `json:"ready"`
	Nodes        int64  `json:"nodes"`
	Edges        int64  `json:"edges"`
	Orphans      int64  `json:"orphans"`
	Deep         int64  `json:"deep"`
	Unresolved   int64  `json:"unresolved"`
	Truncated    int64  `json:"truncated"`
}

type siteGraphEdge struct {
	From        int64   `json:"from"`
	Ordinal     int     `json:"ordinal"`
	To          *int64  `json:"to"`
	Source      string  `json:"source"`
	Destination string  `json:"destination"`
	Anchor      string  `json:"anchor"`
	Internal    bool    `json:"internal"`
	Nofollow    bool    `json:"nofollow"`
	Kind        string  `json:"kind"`
	HTTP        *int    `json:"http"`
	Status      *string `json:"status"`
}

var graphFilters = map[string]string{
	"all": "TRUE", "internal": "e.is_internal", "external": "NOT e.is_internal",
	"broken": "e.is_internal AND r.status_code>=400", "redirects": "e.is_internal AND r.status_code BETWEEN 300 AND 399",
	"unresolved": "e.is_internal AND r.target_id IS NULL", "nofollow": "e.nofollow",
}

func (s *webServer) graph(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	if _, err := loadWebRun(r.Context(), s.pool, s.cfg, id); err != nil {
		webDBError(w, err)
		return
	}
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "all"
	}
	predicate, ok := graphFilters[filter]
	if !ok {
		writeWebError(w, 400, "invalid_filter", "Невідомий фільтр графа")
		return
	}
	var after, target int64
	var ordinal int64
	for _, field := range []struct {
		name  string
		value *int64
	}{{"after", &after}, {"ordinal", &ordinal}, {"target", &target}} {
		if q.Get(field.name) == "" {
			continue
		}
		value, err := strconv.ParseInt(q.Get(field.name), 10, 64)
		if err != nil || value < 0 {
			writeWebError(w, 400, "invalid_cursor", "Некоректний cursor графа")
			return
		}
		*field.value = value
	}
	var info siteGraphInfo
	err := s.pool.QueryRow(r.Context(), `SELECT s.root_safe_url,s.max_pages,s.max_depth,s.sitemap_state,s.discovery_warning,s.limit_reached,s.graph_ready,
		(SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=s.run_id),
		(SELECT COUNT(*) FROM audit_site_edges WHERE run_id=s.run_id),
		(SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=s.run_id AND orphan_candidate),
		(SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=s.run_id AND crawl_depth>3),
		(SELECT COUNT(*) FROM audit_site_edges e LEFT JOIN audit_site_nodes n ON n.run_id=e.run_id AND n.url_fingerprint=e.to_fingerprint WHERE e.run_id=s.run_id AND e.is_internal AND n.target_id IS NULL),
		(SELECT COUNT(*) FROM audit_site_nodes WHERE run_id=s.run_id AND links_truncated)
		FROM audit_site_crawls s WHERE s.run_id=$1`, id).Scan(&info.Root, &info.MaxPages, &info.MaxDepth, &info.SitemapState, &info.Warning, &info.Limited, &info.Ready, &info.Nodes, &info.Edges, &info.Orphans, &info.Deep, &info.Unresolved, &info.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		writeWebJSON(w, 200, map[string]any{"site": nil, "edges": []siteGraphEdge{}, "more": false})
		return
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	info.Root = redactURL(info.Root)
	info.Warning = redactText(info.Warning)
	rows, err := s.pool.Query(r.Context(), `SELECT e.from_target_id,e.ordinal,n.target_id,src.safe_url,e.safe_url,e.anchor_text,e.is_internal,e.nofollow,e.kind,r.status_code,r.scan_status
		FROM audit_site_edges e JOIN audit_site_nodes src ON src.run_id=e.run_id AND src.target_id=e.from_target_id
		LEFT JOIN audit_site_nodes n ON n.run_id=e.run_id AND n.url_fingerprint=e.to_fingerprint
		LEFT JOIN audit_results r ON r.run_id=n.run_id AND r.target_id=n.target_id
		WHERE e.run_id=$1 AND (e.from_target_id,e.ordinal)>($2,$3) AND ($4::BIGINT=0 OR e.from_target_id=$4 OR n.target_id=$4)
		AND (`+predicate+`) ORDER BY e.from_target_id,e.ordinal LIMIT 51`, id, after, ordinal, target)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	edges := make([]siteGraphEdge, 0, 50)
	more := false
	for rows.Next() {
		if len(edges) == 50 {
			more = true
			break
		}
		var e siteGraphEdge
		if err = rows.Scan(&e.From, &e.Ordinal, &e.To, &e.Source, &e.Destination, &e.Anchor, &e.Internal, &e.Nofollow, &e.Kind, &e.HTTP, &e.Status); err != nil {
			webDBError(w, err)
			return
		}
		e.Source = redactURL(e.Source)
		e.Destination = redactURL(e.Destination)
		e.Anchor = redactText(e.Anchor)
		edges = append(edges, e)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"site": info, "edges": edges, "more": more})
}
