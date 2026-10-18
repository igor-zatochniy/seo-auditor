package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func insightOriginForms(ctx context.Context, tx pgx.Tx, id, origin string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT substring(safe_url FROM '^https?://[^/?#]+') FROM audit_results WHERE run_id=$1`, id)
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
			normalized, e := searchOrigin(*raw)
			if e == nil && normalized == origin {
				forms = append(forms, *raw)
			}
		}
	}
	return forms, rows.Err()
}

func structurePrefix(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	if !utf8.ValidString(raw) || len(raw) > 2048 || !strings.HasPrefix(raw, "/") || !strings.HasSuffix(raw, "/") || strings.ContainsFunc(raw, unicode.IsControl) {
		return "", fmt.Errorf("шлях розділу має починатися та закінчуватися / і містити до 2048 байтів")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.IsAbs() || u.RawQuery != "" || u.Fragment != "" || strings.HasPrefix(raw, "//") {
		return "", fmt.Errorf("некоректний шлях розділу")
	}
	return raw, nil
}

const structureSQL = `WITH search AS (
 SELECT s.fingerprint,i.fingerprint_key_id,
 SUM(s.impressions) FILTER(WHERE s.day=$4::date) impressions,
 SUM(s.clicks) FILTER(WHERE s.day=$4::date) clicks,
 SUM(s.clicks) FILTER(WHERE s.day=$5::date) previous_clicks,
 jsonb_agg(jsonb_build_array(s.query_key,s.country,s.device) ORDER BY s.query_key,s.country,s.device) FILTER(WHERE s.day=$4::date)
 =jsonb_agg(jsonb_build_array(s.query_key,s.country,s.device) ORDER BY s.query_key,s.country,s.device) FILTER(WHERE s.day=$5::date) comparable
 FROM audit_search_rows s JOIN audit_search_imports i ON i.id=s.import_id
 WHERE s.run_id=$1 AND s.origin=$2 AND s.day IN ($4::date,$5::date)
 AND ($8='' OR s.country=$8) AND ($9='' OR s.device=$9)
 GROUP BY s.fingerprint,i.fingerprint_key_id
), bots AS (
 SELECT e.target_fingerprint,i.fingerprint_key_id,COUNT(*) visits,COUNT(*) FILTER(WHERE verification='matched_range') verified,
 COUNT(*) FILTER(WHERE http_status>=400) errors
 FROM audit_bot_events e JOIN audit_bot_imports i ON i.id=e.import_id WHERE i.id::text=$6
 GROUP BY e.target_fingerprint,i.fingerprint_key_id
), geo AS (SELECT target_id,COUNT(*) queries FROM unnest($7::bigint[]) target_id GROUP BY target_id), paths AS (
 SELECT a.*,COALESCE(NULLIF(regexp_replace(split_part(split_part(a.safe_url,'?',1),'#',1),'^https?://[^/]+',''),''),'/') path,
 s.impressions,s.clicks,s.previous_clicks,s.comparable,b.visits,b.verified,b.errors bot_errors,g.queries
 FROM audit_results a LEFT JOIN search s ON s.fingerprint=a.target_fingerprint AND s.fingerprint_key_id=a.fingerprint_key_id
 LEFT JOIN bots b ON b.target_fingerprint=a.target_fingerprint AND b.fingerprint_key_id=a.fingerprint_key_id
 LEFT JOIN geo g ON g.target_id=a.target_id
 WHERE a.run_id=$1 AND substring(a.safe_url FROM '^https?://[^/?#]+')=ANY($11::text[])
), scoped AS (
 SELECT *,CASE WHEN path=$3 OR path=rtrim($3,'/') THEN $3
 ELSE $3||split_part(substring(path FROM char_length($3)+1),'/',1)||'/' END section
 FROM paths WHERE left(path,char_length($3))=$3 OR path=rtrim($3,'/')
)
 SELECT jsonb_build_object('path',section,'pages',COUNT(*),'children',bool_or(path<>section AND path<>rtrim(section,'/')),
 'http_errors',COUNT(*) FILTER(WHERE status_code>=400),'failed',COUNT(*) FILTER(WHERE scan_status='failed'),
 'robots_blocked',COUNT(*) FILTER(WHERE scan_status='blocked_by_robots'),
 'noindex',COUNT(*) FILTER(WHERE geo_signals->'search'->>'google_indexing'='blocked'),
 'indexing_unknown',COUNT(*) FILTER(WHERE geo_signals->'search'->>'google_indexing' IS NULL OR geo_signals->'search'->>'google_indexing'='unknown'),
 'nonself_canonical',COUNT(*) FILTER(WHERE is_self_canonical=FALSE),'geo_pages',COUNT(*) FILTER(WHERE queries>0),'geo_queries',COALESCE(SUM(queries),0),
 'gsc_pages',COUNT(impressions),'impressions',SUM(impressions),'clicks',SUM(clicks),'previous_clicks',SUM(previous_clicks),
 'traffic_comparable',COUNT(clicks)>0 AND COUNT(clicks)=COUNT(previous_clicks) AND COUNT(clicks)=COUNT(*) FILTER(WHERE clicks IS NOT NULL AND previous_clicks IS NOT NULL) AND COALESCE(bool_and(comparable) FILTER(WHERE clicks IS NOT NULL OR previous_clicks IS NOT NULL),FALSE),
 'bot_visits',CASE WHEN $6<>'' THEN COALESCE(SUM(visits),0) END,
 'verified_bot_visits',CASE WHEN $6<>'' THEN COALESCE(SUM(verified),0) END,
 'bot_errors',CASE WHEN $6<>'' THEN COALESCE(SUM(bot_errors),0) END)
 FROM scoped GROUP BY section ORDER BY section COLLATE "C" LIMIT 51 OFFSET $10`

func (s *webServer) siteStructure(w http.ResponseWriter, r *http.Request) {
	prefix, err := structurePrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		writeWebError(w, 400, "invalid_prefix", err.Error())
		return
	}
	offset, err := insightOffset(r)
	if err != nil {
		writeWebError(w, 400, "invalid_cursor", err.Error())
		return
	}
	tx, id, origin, ok := s.insightRead(w, r)
	if !ok {
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	day := r.URL.Query().Get("day")
	if day != "" {
		if _, e := time.Parse("2006-01-02", day); e != nil {
			writeWebError(w, 400, "invalid_day", "Дата має формат YYYY-MM-DD")
			return
		}
	}
	country, device, filterErr := searchFilters(r)
	if filterErr != nil {
		writeWebError(w, 400, "invalid_segment", filterErr.Error())
		return
	}
	var selectedDay, previousDay *string
	err = tx.QueryRow(r.Context(), `SELECT MAX(day)::text FROM audit_search_rows WHERE run_id=$1 AND origin=$2 AND ($3='' OR country=$3) AND ($4='' OR device=$4) AND ($5='' OR day::text=$5)`, id, origin, country, device, day).Scan(&selectedDay)
	if err != nil {
		webDBError(w, err)
		return
	}
	if selectedDay != nil {
		err = tx.QueryRow(r.Context(), `SELECT MAX(day)::text FROM audit_search_rows WHERE run_id=$1 AND origin=$2 AND ($3='' OR country=$3) AND ($4='' OR device=$4) AND day<$5::date`, id, origin, country, device, *selectedDay).Scan(&previousDay)
		if err != nil {
			webDBError(w, err)
			return
		}
	}
	reportID, geoRows, err := insightGEO(r.Context(), tx, id, origin, r.URL.Query().Get("geo_report_id"))
	if err != nil {
		webDBError(w, err)
		return
	}
	targets := []int64{}
	unmapped := 0
	for _, v := range geoRows {
		if v.Result.TargetID != nil {
			targets = append(targets, *v.Result.TargetID)
		} else {
			unmapped++
		}
	}
	botID := ""
	err = tx.QueryRow(r.Context(), `SELECT bot_import_id::text FROM audit_crawl_source_imports WHERE run_id=$1 AND source='bots' ORDER BY created_at DESC,id DESC LIMIT 1`, id).Scan(&botID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	var meta map[string]any
	err = tx.QueryRow(r.Context(), `SELECT jsonb_build_object('status',r.status,'snapshot_targets',r.total_urls,'saved_results',(SELECT COUNT(*) FROM audit_results WHERE run_id=r.id),
 'site_crawl',c.run_id IS NOT NULL,'partial',c.run_id IS NULL OR c.limit_reached OR NOT c.graph_ready OR c.sitemap_state IN ('pending','partial') OR r.status<>'completed' OR (SELECT COUNT(*) FROM audit_results WHERE run_id=r.id)<r.total_urls
 OR EXISTS(SELECT 1 FROM audit_site_nodes WHERE run_id=r.id AND links_truncated),'warning',c.discovery_warning)
 FROM audit_runs r LEFT JOIN audit_site_crawls c ON c.run_id=r.id WHERE r.id=$1`, id).Scan(&meta)
	if err != nil {
		webDBError(w, err)
		return
	}
	forms, err := insightOriginForms(r.Context(), tx, id, origin)
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), structureSQL, id, origin, prefix, selectedDay, previousDay, botID, targets, country, device, offset, forms)
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
	writeWebJSON(w, 200, map[string]any{"origin": origin, "prefix": prefix, "items": items, "more": more, "offset": offset, "day": selectedDay, "previous_day": previousDay,
		"geo_report_id": reportID, "unmapped_queries": unmapped, "bot_import_id": botID, "meta": meta,
		"note": "Розділи містять збережені результати цього аудиту, не всі URL сайту. GEO — зіставлені запити останнього звіту цього origin з урахуванням ручних цілей, не AI-видимість. GSC — сума імпортованих рядків URL; відсутні URL не вважаються нульовими. Порівняння кліків доступне лише для однакового набору сторінок, запитів, країн і пристроїв. Журнали — останній явно прикріплений до аудиту імпорт; непобачений візит не доводить відсутність доступу. HTTP-помилки та failed можуть перетинатися; non-self canonical не є помилкою сам по собі."})
}
