package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/jackc/pgx/v5"
)

const routePageSize = 25
const routeNodeBudget = 512

type routePage struct {
	TargetID  int64            `json:"target_id"`
	URL       string           `json:"url"`
	Redirect  crawler.URLRoute `json:"redirect"`
	Canonical crawler.URLRoute `json:"canonical"`
	Conflicts []string         `json:"conflicts"`
	Inlinks   int64            `json:"inlinks"`
}

const routeNodeColumns = `r.target_id,r.safe_url,encode(r.target_fingerprint,'hex'),r.fingerprint_key_id,r.status_code,r.scan_status,
 COALESCE(r.geo_signals->'search'->>'google_indexing'='blocked',FALSE),COALESCE(n.in_sitemap,FALSE),r.url_signals,
 r.canonical_url,r.international->>'canonical_fingerprint',r.html_size_complete,
 (SELECT encode(e.to_fingerprint,'hex') FROM audit_site_edges e WHERE e.run_id=r.run_id AND e.from_target_id=r.target_id AND e.kind='redirect' ORDER BY e.ordinal LIMIT 1),r.redirect_url,
 EXISTS(SELECT 1 FROM audit_results other WHERE other.run_id=r.run_id AND other.target_fingerprint=r.target_fingerprint AND other.fingerprint_key_id=r.fingerprint_key_id AND other.target_id<>r.target_id)`
const routeNodeJoin = ` FROM audit_results r LEFT JOIN audit_site_nodes n ON n.run_id=r.run_id AND n.target_id=r.target_id `

func readRouteNodes(rows pgx.Rows) ([]crawler.RouteNode, error) {
	defer rows.Close()
	out := []crawler.RouteNode{}
	for rows.Next() {
		var n crawler.RouteNode
		var raw []byte
		var canonical, redirect string
		var canonicalFP, redirectFP *string
		var htmlComplete bool
		if err := rows.Scan(&n.ID, &n.URL, &n.Fingerprint, &n.KeyID, &n.HTTP, &n.Status, &n.Noindex, &n.Sitemap, &raw, &canonical, &canonicalFP, &htmlComplete, &redirectFP, &redirect, &n.Ambiguous); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if len(raw) > 65536 || json.Unmarshal(raw, &n.Signals) != nil {
				return nil, fmt.Errorf("invalid URL signals for target %d", n.ID)
			}
		} else {
			n.Signals = &crawler.URLSignals{Version: "legacy", Complete: false}
			if canonicalFP != nil && len(*canonicalFP) == 64 {
				n.Signals.HTML = &crawler.URLReference{URL: canonical, Fingerprints: []string{*canonicalFP}, Valid: true}
			}
			if redirectFP != nil {
				n.Signals.Redirect = &crawler.URLReference{URL: redirect, Fingerprints: []string{*redirectFP}, Valid: true}
			}
		}
		n.URL = redactURL(n.URL)
		out = append(out, n)
	}
	return out, rows.Err()
}

func routeKey(key, fp string) string { return key + ":" + fp }

func (s *webServer) urlRoutes(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	run, err := loadWebRun(r.Context(), s.pool, s.cfg, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	after, err := strconv.ParseInt("0"+r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeWebError(w, 400, "invalid_cursor", "Некоректний cursor маршрутів")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+routeNodeColumns+routeNodeJoin+` WHERE r.run_id=$1 AND r.target_id>$2 ORDER BY r.target_id LIMIT 26`, id, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	initial, err := readRouteNodes(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	more := len(initial) > routePageSize
	if more {
		initial = initial[:routePageSize]
	}
	cache := map[string]crawler.RouteNode{}
	add := func(n crawler.RouteNode) {
		key := routeKey(n.KeyID, n.Fingerprint)
		if old, exists := cache[key]; exists && old.ID != n.ID {
			old.Ambiguous = true
			cache[key] = old
		} else {
			cache[key] = n
		}
	}
	for _, n := range initial {
		add(n)
	}
	missing := map[string]bool{}
	limited := false
	// Відбираємо тільки сусідів поточного пакета, не весь корпус у RAM.
	for depth := 0; depth < crawler.MaxRouteHops; depth++ {
		pending := map[string]bool{}
		for _, n := range cache {
			for _, mode := range []string{"redirect", "canonical"} {
				ref := n.Reference(mode)
				if ref == nil || !ref.Valid {
					continue
				}
				for _, fp := range ref.Fingerprints {
					key := routeKey(n.KeyID, fp)
					if _, exists := cache[key]; !exists && !missing[key] {
						pending[key] = true
					}
				}
			}
		}
		if len(pending) == 0 {
			break
		}
		if len(cache)+len(pending) > routeNodeBudget {
			limited = true
			break
		}
		keys, fps := []string{}, [][]byte{}
		for key := range pending {
			parts := strings.SplitN(key, ":", 2)
			fp, e := hex.DecodeString(parts[1])
			if e != nil || len(fp) != 32 {
				webDBError(w, fmt.Errorf("invalid route fingerprint"))
				return
			}
			keys = append(keys, parts[0])
			fps = append(fps, fp)
			missing[key] = true
		}
		rows, err = s.pool.Query(r.Context(), `SELECT `+routeNodeColumns+routeNodeJoin+` JOIN unnest($2::TEXT[],$3::BYTEA[]) wanted(key,fp) ON r.fingerprint_key_id=wanted.key AND r.target_fingerprint=wanted.fp WHERE r.run_id=$1 ORDER BY r.target_id LIMIT 513`, id, keys, fps)
		if err != nil {
			webDBError(w, err)
			return
		}
		nodes, e := readRouteNodes(rows)
		if e != nil {
			webDBError(w, e)
			return
		}
		if len(nodes) > routeNodeBudget {
			limited = true
			break
		}
		for _, n := range nodes {
			add(n)
		}
	}
	lookup := func(key, fp string) (crawler.RouteNode, bool) { n, ok := cache[routeKey(key, fp)]; return n, ok }
	items := []routePage{}
	for _, n := range initial {
		item := routePage{TargetID: n.ID, URL: n.URL, Conflicts: []string{}, Redirect: crawler.TraceURLRoute(r.Context(), n, "redirect", lookup), Canonical: crawler.TraceURLRoute(r.Context(), n, "canonical", lookup)}
		if limited {
			if item.Redirect.State == "not_audited" {
				item.Redirect.State = "lookup_limit"
			}
			if item.Canonical.State == "not_audited" {
				item.Canonical.State = "lookup_limit"
			}
		}
		if n.Signals != nil {
			refs := n.Signals.HTTP
			if n.Signals.HTML != nil {
				refs = append(append([]crawler.URLReference(nil), refs...), *n.Signals.HTML)
			}
			var known *crawler.URLReference
			for i := range refs {
				ref := refs[i]
				if ref.Valid && len(ref.Fingerprints) > 0 {
					if known != nil && !referencesIntersect(*known, ref) {
						item.Conflicts = append(item.Conflicts, "canonical_sources_disagree")
						break
					}
					known = &ref
				}
			}
			if n.Sitemap && n.Signals.HTML != nil && n.Signals.HTML.Valid {
				self := false
				for _, fp := range n.Signals.HTML.Fingerprints {
					self = self || fp == n.Fingerprint
				}
				if !self {
					item.Conflicts = append(item.Conflicts, "sitemap_noncanonical_candidate")
				}
			}
		}
		items = append(items, item)
	}
	ids := []int64{}
	for _, item := range items {
		ids = append(ids, item.TargetID)
	}
	counts, err := s.pool.Query(r.Context(), `SELECT n.target_id,COUNT(DISTINCT e.from_target_id) FROM audit_site_nodes n LEFT JOIN audit_site_edges e ON e.run_id=n.run_id AND e.to_fingerprint=n.url_fingerprint AND e.kind='link' WHERE n.run_id=$1 AND n.target_id=ANY($2::BIGINT[]) GROUP BY n.target_id`, id, ids)
	if err != nil {
		webDBError(w, err)
		return
	}
	values := map[int64]int64{}
	for counts.Next() {
		var target, count int64
		if err = counts.Scan(&target, &count); err != nil {
			counts.Close()
			webDBError(w, err)
			return
		}
		values[target] = count
	}
	err = counts.Err()
	counts.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	for i := range items {
		items[i].Inlinks = values[items[i].TargetID]
	}
	next := int64(0)
	if more && len(items) > 0 {
		next = items[len(items)-1].TargetID
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next, "limited": limited, "max_hops": crawler.MaxRouteHops, "run_status": run.Status})
}

func referencesIntersect(a, b crawler.URLReference) bool {
	for _, left := range a.Fingerprints {
		for _, right := range b.Fingerprints {
			if left == right {
				return true
			}
		}
	}
	return false
}

func (s *webServer) routeInlinks(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	target, err := strconv.ParseInt(r.PathValue("target"), 10, 64)
	if err != nil || target < 1 {
		writeWebError(w, 400, "invalid_target", "Некоректний target ID")
		return
	}
	after, err := strconv.ParseInt("0"+r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeWebError(w, 400, "invalid_cursor", "Некоректний cursor")
		return
	}
	if _, err = loadWebRun(r.Context(), s.pool, s.cfg, id); err != nil {
		webDBError(w, err)
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT src.target_id,src.safe_url FROM audit_site_nodes dst JOIN audit_site_edges e ON e.run_id=dst.run_id AND e.to_fingerprint=dst.url_fingerprint AND e.kind='link' JOIN audit_site_nodes src ON src.run_id=e.run_id AND src.target_id=e.from_target_id WHERE dst.run_id=$1 AND dst.target_id=$2 AND src.target_id>$3 GROUP BY src.target_id,src.safe_url ORDER BY src.target_id LIMIT 51`, id, target, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	next := int64(0)
	for rows.Next() {
		var source int64
		var address string
		if err = rows.Scan(&source, &address); err != nil {
			webDBError(w, err)
			return
		}
		if len(items) == 50 {
			next = items[49]["target_id"].(int64)
			break
		}
		items = append(items, map[string]any{"target_id": source, "url": redactURL(address)})
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next})
}
