package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/publicsuffix"
)

func cdnOriginForms(ctx context.Context, tx pgx.Tx, id, origin string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT substring(safe_url FROM '^https?://[^/?#]+') FROM audit_bot_events WHERE import_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	forms := []string{}
	for rows.Next() {
		var raw *string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if raw != nil {
			actual, e := searchOrigin(*raw)
			if e == nil && actual == origin {
				forms = append(forms, *raw)
			}
		}
	}
	return forms, rows.Err()
}

func (s *webServer) cdnDiagnostics(w http.ResponseWriter, r *http.Request) {
	tx, id, origin, ok := s.insightRead(w, r)
	if !ok {
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	offset, err := insightOffset(r)
	if err != nil {
		writeWebError(w, 400, "invalid_cursor", err.Error())
		return
	}
	u, _ := url.Parse(origin)
	domains := []string{u.Hostname()}
	if registered, e := publicsuffix.EffectiveTLDPlusOne(u.Hostname()); e == nil {
		for host := u.Hostname(); host != registered; {
			_, host, _ = strings.Cut(host, ".")
			domains = append(domains, host)
		}
	}
	rows, err := tx.Query(r.Context(), `SELECT jsonb_build_object('id',id,'format',format,'stats',stats,'created_at',created_at) FROM audit_bot_imports
 WHERE domain=ANY($1::text[]) AND format IN ('cloudfront','cloudflare','vercel','fastly') ORDER BY created_at DESC,id DESC LIMIT 100`, domains)
	if err != nil {
		webDBError(w, err)
		return
	}
	imports, err := diagnosticRows(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	selected := r.URL.Query().Get("import_id")
	if selected == "" && len(imports) > 0 {
		selected = imports[0]["id"].(string)
	}
	if selected == "" {
		writeWebJSON(w, 200, map[string]any{"imports": imports, "items": []any{}, "origin": origin, "more": false})
		return
	}
	if !runIDPattern.MatchString(selected) {
		writeWebError(w, 400, "invalid_import", "Некоректний ідентифікатор імпорту")
		return
	}
	var meta map[string]any
	err = tx.QueryRow(r.Context(), `SELECT jsonb_build_object('id',id,'format',format,'stats',stats,'created_at',created_at,'range_evidence',range_evidence) FROM audit_bot_imports WHERE id=$1 AND domain=ANY($2::text[]) AND format IN ('cloudfront','cloudflare','vercel','fastly')`, selected, domains).Scan(&meta)
	if err != nil {
		webDBError(w, err)
		return
	}
	forms, err := cdnOriginForms(r.Context(), tx, selected, origin)
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err = tx.Query(r.Context(), `WITH events AS(
 SELECT MIN(e.ordinal) cursor,e.target_fingerprint,e.safe_url,e.bot,e.verification,COUNT(*) visits,
 COUNT(*) FILTER(WHERE e.http_status IN (403,429,503)) rejects,
 COUNT(*) FILTER(WHERE e.http_status=429) rate_limits,COUNT(*) FILTER(WHERE e.http_status=503) unavailable,
 COUNT(*) FILTER(WHERE e.http_status>=400 AND e.origin_status BETWEEN 200 AND 299) edge_origin_discrepancies,
 ARRAY_AGG(DISTINCT e.http_status ORDER BY e.http_status) edge_statuses,
 ARRAY_AGG(DISTINCT e.origin_status ORDER BY e.origin_status) FILTER(WHERE e.origin_status IS NOT NULL) origin_statuses,
 ARRAY_AGG(DISTINCT e.cache_status ORDER BY e.cache_status) FILTER(WHERE e.cache_status IS NOT NULL) caches,
 MIN(e.observed_at) first_seen,MAX(e.observed_at) last_seen,i.fingerprint_key_id
 FROM audit_bot_events e JOIN audit_bot_imports i ON i.id=e.import_id
 WHERE e.import_id=$1 GROUP BY e.target_fingerprint,e.safe_url,e.bot,e.verification,i.fingerprint_key_id
 ) SELECT jsonb_build_object('cursor',e.cursor,'url',e.safe_url,'bot',e.bot,'verification',e.verification,'visits',e.visits,'rejects',e.rejects,
 'repeated_429_503',e.rate_limits>1 OR e.unavailable>1,'edge_origin_discrepancies',e.edge_origin_discrepancies,
 'edge_statuses',e.edge_statuses,'origin_statuses',e.origin_statuses,'cache_statuses',e.caches,'first',e.first_seen,'last',e.last_seen,
 'audit_http',a.status_code,'audit_scan_status',a.scan_status,'target_id',a.target_id,
 'audit_discrepancy',CASE WHEN a.target_id IS NOT NULL THEN a.status_code=200 AND e.rejects>0 END)
 FROM events e LEFT JOIN audit_results a ON a.run_id=$2 AND a.target_fingerprint=e.target_fingerprint AND a.fingerprint_key_id=e.fingerprint_key_id
 WHERE substring(e.safe_url FROM '^https?://[^/?#]+')=ANY($3::text[]) ORDER BY e.cursor LIMIT 51 OFFSET $4`, selected, id, forms, offset)
	if err != nil {
		webDBError(w, err)
		return
	}
	items, err := diagnosticRows(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	more := len(items) > 50
	if more {
		items = items[:50]
	}
	writeWebJSON(w, 200, map[string]any{"origin": origin, "import": meta, "imports": imports, "items": items, "more": more, "note": "Імпортовані журнали є наданими користувачем спостереженнями, не мережевою симуляцією. IP перевіряється за збереженим актуальним списком, не криптографічно. Немає origin/cache — невідомо, а не 0/MISS. Аудит і журнал можуть мати різні дати, IP, cookies і User-Agent: розбіжність не доводить причину WAF. Імпорти не підсумовуються; повторні request ID всередині файлу усуваються, файли з перекриттям порівнюйте окремо. У списку останні 100 CDN-імпортів домену."})
}
