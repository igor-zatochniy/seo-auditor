package main

import (
	"github.com/igor-zatochniy/seo-auditor/internal/duplicates"
	"net/http"
	"strings"
)

func facetWarnings(variants, duplicateExcess, nonSelf, queryKeys int, maxPage int64) []string {
	reasons := []string{}
	if variants >= 20 && duplicateExcess*2 >= variants {
		reasons = append(reasons, "duplicate_variants")
	}
	if variants >= 20 && nonSelf*2 >= variants {
		reasons = append(reasons, "noncanonical_variants")
	}
	if variants >= 50 && queryKeys >= 3 {
		reasons = append(reasons, "parameter_combinations")
	}
	if variants >= 20 && maxPage >= 1000 {
		reasons = append(reasons, "deep_pagination")
	}
	return reasons
}

func (s *webServer) crawlFacets(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.coverageSite(w, r, false)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if len(after) > 2048 || strings.ContainsRune(after, 0) {
		writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
		return
	}
	rows, e := s.pool.Query(r.Context(), coverageSQL+`, groups AS (
 SELECT url_shape->>'family' family,COUNT(*) variants,COUNT(*) FILTER(WHERE scan_status IS NOT NULL) audited,
 COUNT(*) FILTER(WHERE indexing='allowed') indexable,COUNT(*) FILTER(WHERE indexing='blocked') noindex,
 SUM(bot_visits) bot_entries,SUM(matched_range_visits) matched_range_entries,
 COUNT(*) FILTER(WHERE NOT COALESCE((url_shape->>'complete')::BOOLEAN,FALSE)) unknown_shapes,
 MAX(COALESCE((url_shape->>'page_bucket')::BIGINT,0)) max_page,
 MAX(jsonb_array_length(url_shape->'keys')) max_keys
 FROM data WHERE (url_shape->>'has_query')::BOOLEAN AND COALESCE(url_shape->>'family','')<>'' GROUP BY url_shape->>'family'
), signatures AS (
 SELECT i.url_shape->>'family' family,
 COUNT(*) FILTER(WHERE r.content_signature->>'model'=$3 AND (r.content_signature->>'complete')::BOOLEAN AND (r.content_signature->>'words')::INT>=50)
 - COUNT(DISTINCT r.content_signature->>'sha256') FILTER(WHERE r.content_signature->>'model'=$3 AND (r.content_signature->>'complete')::BOOLEAN AND (r.content_signature->>'words')::INT>=50) duplicate_excess,
 COUNT(*) FILTER(WHERE r.canonical_url<>'' AND NOT r.canonical_url_truncated AND NOT r.is_self_canonical) nonself,
 COUNT(*) FILTER(WHERE r.canonical_url<>'') canonical_known
 FROM audit_crawl_inventory i LEFT JOIN audit_site_nodes n ON n.run_id=i.run_id AND n.url_fingerprint=i.fingerprint
 LEFT JOIN audit_results r ON r.run_id=n.run_id AND r.target_id=n.target_id WHERE i.run_id=$1 GROUP BY i.url_shape->>'family'
)
 SELECT jsonb_build_object('cursor',g.family,'family',g.family,'variants',g.variants,'audited',g.audited,'indexable',g.indexable,'noindex',g.noindex,
 'bot_entries',g.bot_entries,'matched_range_entries',g.matched_range_entries,'max_pagination',g.max_page,'max_keys',g.max_keys,'unknown_shapes',g.unknown_shapes,
 'duplicate_excess',COALESCE(s.duplicate_excess,0),'nonself',COALESCE(s.nonself,0),'canonical_known',COALESCE(s.canonical_known,0))
 FROM groups g LEFT JOIN signatures s ON s.family=g.family WHERE g.family>$2 ORDER BY g.family LIMIT 51`, id, after, duplicates.Model)
	if e != nil {
		webDBError(w, e)
		return
	}
	items, e := diagnosticRows(rows)
	if e != nil {
		webDBError(w, e)
		return
	}
	next := ""
	if len(items) > 50 {
		next = items[49]["cursor"].(string)
		items = items[:50]
	}
	for _, v := range items {
		n := func(key string) int { value, _ := v[key].(float64); return int(value) }
		v["reviews"] = facetWarnings(n("variants"), n("duplicate_excess"), n("nonself"), n("max_keys"), int64(n("max_pagination")))
	}
	var limited, legacy bool
	var excluded int
	e = s.pool.QueryRow(r.Context(), `SELECT COALESCE(i.limited,FALSE) OR c.limit_reached OR c.sitemap_state IN ('pending','partial') OR NOT c.graph_ready OR r.status<>'completed'
 OR EXISTS(SELECT 1 FROM audit_site_nodes n WHERE n.run_id=c.run_id AND n.links_truncated)
 OR EXISTS(SELECT 1 FROM audit_crawl_inventory v WHERE v.run_id=c.run_id AND (v.url_shape->>'has_query')::BOOLEAN AND NOT COALESCE((v.url_shape->>'complete')::BOOLEAN,FALSE)),
		NOT COALESCE(i.captured,FALSE),
 (SELECT COUNT(*) FROM audit_crawl_inventory v WHERE v.run_id=c.run_id AND (v.url_shape->>'has_query')::BOOLEAN AND COALESCE(v.url_shape->>'family','')='')
 FROM audit_site_crawls c JOIN audit_runs r ON r.id=c.run_id LEFT JOIN audit_crawl_inventory_state i ON i.run_id=c.run_id WHERE c.run_id=$1`, id).Scan(&limited, &legacy, &excluded)
	if e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next, "partial": limited || legacy, "legacy": legacy, "excluded_shapes": excluded})
}
