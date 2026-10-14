package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/jackc/pgx/v5"
)

var errCoverageRunActive = errors.New("аудит уже виконується")

func lockCoverageDestination(ctx context.Context, tx pgx.Tx, id string) error {
	var status string
	if e := tx.QueryRow(ctx, `SELECT status FROM audit_runs WHERE id=$1 FOR UPDATE`, id).Scan(&status); e != nil {
		return e
	}
	if status == auditRunStatusRunning {
		return errCoverageRunActive
	}
	return nil
}
func coverageMutationError(w http.ResponseWriter, e error) {
	if errors.Is(e, errCoverageRunActive) {
		writeWebError(w, 409, "audit_running", "Дочекайтеся завершення аудиту")
	} else {
		webDBError(w, e)
	}
}

func (s *webServer) registerCrawlDiagnostics(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audits/{id}/coverage", s.crawlCoverage)
	mux.HandleFunc("POST /api/audits/{id}/coverage/gsc", s.importCoverageGSC)
	mux.HandleFunc("POST /api/audits/{id}/coverage/bots", s.attachCoverageBots)
	mux.HandleFunc("POST /api/audits/{id}/coverage/priority", s.coveragePriority)
	mux.HandleFunc("GET /api/audits/{id}/facets", s.crawlFacets)
}

func (s *webServer) coverageSite(w http.ResponseWriter, r *http.Request, write bool) (string, string, bool) {
	id, ok := webRunID(w, r)
	if !ok {
		return "", "", false
	}
	var origin, status string
	var check []byte
	e := s.pool.QueryRow(r.Context(), `SELECT c.origin,c.key_check,r.status FROM audit_site_crawls c JOIN audit_runs r ON r.id=c.run_id WHERE c.run_id=$1`, id).Scan(&origin, &check, &status)
	if errors.Is(e, pgx.ErrNoRows) {
		writeWebError(w, 409, "site_crawl_required", "Потрібен аудит у режимі Site Crawl")
		return "", "", false
	}
	if e != nil {
		webDBError(w, e)
		return "", "", false
	}
	if write && (status == auditRunStatusRunning || !bytes.Equal(check, fingerprintURL(s.cfg.TargetFingerprintKey, "site-crawl:"+id))) {
		writeWebError(w, 409, "coverage_unavailable", "Дочекайтеся завершення та використовуйте початковий HMAC-ключ")
		return "", "", false
	}
	return id, origin, true
}

const coverageSQL = `WITH known AS (
 SELECT fingerprint,safe_url,sources,deferred,important,url_shape FROM audit_crawl_inventory WHERE run_id=$1
 UNION ALL SELECT n.url_fingerprint,n.safe_url,CASE WHEN n.in_sitemap THEN 2 ELSE 0 END,0,FALSE,NULL::JSONB FROM audit_site_nodes n
 WHERE n.run_id=$1 AND NOT EXISTS(SELECT 1 FROM audit_crawl_inventory i WHERE i.run_id=n.run_id AND i.fingerprint=n.url_fingerprint)
), bots AS (SELECT e.fingerprint,SUM(e.visits) visits,SUM(e.matched_range_visits) matched,MIN(e.first_seen) first_seen,MAX(e.last_seen) last_seen
 FROM audit_crawl_bot_evidence e JOIN audit_crawl_source_imports s ON s.id=e.source_id WHERE s.run_id=$1 GROUP BY e.fingerprint), data AS (
 SELECT k.*,n.target_id,r.status_code,r.scan_status,r.error_code,r.robots_outcome,
 COALESCE(n.internal_inlinks_count,links.inlinks,0) inlinks,COALESCE(b.visits,0) bot_visits,COALESCE(b.matched,0) matched_range_visits,b.first_seen,b.last_seen,
 CASE WHEN r.scan_status='blocked_by_robots' THEN 'robots' WHEN r.scan_status='failed' OR r.status_code>=400 THEN 'error'
 WHEN r.target_id IS NOT NULL THEN 'scanned' WHEN t.status IN ('pending','running') THEN 'pending'
 WHEN n.target_id IS NOT NULL THEN 'missing_result' WHEN (k.deferred&1)>0 THEN 'depth_limit' WHEN (k.deferred&2)>0 THEN 'page_limit'
 WHEN (k.deferred&4)>0 THEN 'nofollow' WHEN (k.sources&2)>0 AND c.limit_reached THEN 'page_limit' ELSE 'not_discovered' END state,
 (k.sources&14)>0 AND k.fingerprint<>(SELECT url_fingerprint FROM audit_site_nodes WHERE run_id=$1 AND target_id=1) AND COALESCE(n.internal_inlinks_count,links.inlinks,0)=0 orphan_candidate,
 r.geo_signals->'search'->>'google_indexing' indexing
 FROM known k JOIN audit_site_crawls c ON c.run_id=$1 LEFT JOIN audit_site_nodes n ON n.run_id=$1 AND n.url_fingerprint=k.fingerprint
 LEFT JOIN audit_run_targets t ON t.run_id=n.run_id AND t.target_id=n.target_id LEFT JOIN audit_results r ON r.run_id=n.run_id AND r.target_id=n.target_id
 LEFT JOIN bots b ON b.fingerprint=k.fingerprint
 LEFT JOIN LATERAL (SELECT COUNT(DISTINCT e.from_target_id) inlinks FROM audit_site_edges e WHERE e.run_id=$1 AND e.to_fingerprint=k.fingerprint AND e.kind='link') links ON n.internal_inlinks_count IS NULL
) `

func (s *webServer) crawlCoverage(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.coverageSite(w, r, false)
	if !ok {
		return
	}
	after := []byte{}
	if raw := r.URL.Query().Get("after"); raw != "" {
		var e error
		after, e = hex.DecodeString(raw)
		if e != nil || len(after) != 32 {
			writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
			return
		}
	}
	filter := r.URL.Query().Get("filter")
	switch filter {
	case "", "missing", "orphan", "important", "error":
	default:
		writeWebError(w, 400, "invalid_filter", "Некоректний фільтр")
		return
	}
	var summary map[string]any
	e := s.pool.QueryRow(r.Context(), coverageSQL+`SELECT jsonb_build_object('known',COUNT(*),'scanned',COUNT(*) FILTER(WHERE state='scanned'),
 'missing',COUNT(*) FILTER(WHERE state NOT IN ('scanned','pending')),'orphan_candidates',COUNT(*) FILTER(WHERE orphan_candidate),
 'sitemap',COUNT(*) FILTER(WHERE (sources&2)>0),'gsc',COUNT(*) FILTER(WHERE (sources&4)>0),'bots',COUNT(*) FILTER(WHERE (sources&8)>0),
 'important_recheck',COUNT(*) FILTER(WHERE important AND state<>'scanned')) FROM data`, id).Scan(&summary)
	if e != nil {
		webDBError(w, e)
		return
	}
	var meta map[string]any
	e = s.pool.QueryRow(r.Context(), `SELECT jsonb_build_object('inventory_limit',50000,'inventory_limited',COALESCE(i.limited,FALSE),'legacy',NOT COALESCE(i.captured,FALSE),
 'sitemap_state',c.sitemap_state,'limit_reached',c.limit_reached,'graph_ready',c.graph_ready,'warning',c.discovery_warning,
 'partial',NOT COALESCE(i.captured,FALSE) OR COALESCE(i.limited,FALSE) OR c.limit_reached OR NOT c.graph_ready OR c.sitemap_state IN ('pending','partial') OR r.status<>'completed'
 OR EXISTS(SELECT 1 FROM audit_site_nodes n WHERE n.run_id=c.run_id AND n.links_truncated),
 'imports',(SELECT COALESCE(jsonb_agg(jsonb_build_object('id',s.id,'source',s.source,'rows',s.row_count,'created_at',s.created_at) ORDER BY s.created_at), '[]'::JSONB) FROM audit_crawl_source_imports s WHERE s.run_id=c.run_id))
 FROM audit_site_crawls c JOIN audit_runs r ON r.id=c.run_id LEFT JOIN audit_crawl_inventory_state i ON i.run_id=c.run_id WHERE c.run_id=$1`, id).Scan(&meta)
	if e != nil {
		webDBError(w, e)
		return
	}
	rows, e := s.pool.Query(r.Context(), coverageSQL+`SELECT jsonb_build_object('cursor',encode(fingerprint,'hex'),'url',safe_url,'target_id',target_id,'sources',sources,
 'state',state,'important',important,'orphan_candidate',orphan_candidate,'inlinks',inlinks,'http',status_code,'scan_status',scan_status,'error',error_code,
 'indexing',indexing,'bot_visits',bot_visits,'matched_range_visits',matched_range_visits,'first_seen',first_seen,'last_seen',last_seen)
 FROM data WHERE fingerprint>$2 AND ($3='' OR $3='missing' AND state NOT IN ('scanned','pending') OR $3='orphan' AND orphan_candidate
 OR $3='important' AND important OR $3='error' AND state IN ('robots','error','missing_result')) ORDER BY fingerprint LIMIT 51`, id, after, filter)
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
	writeWebJSON(w, 200, map[string]any{"summary": summary, "meta": meta, "items": items, "next": next})
}

func diagnosticRows(rows pgx.Rows) ([]map[string]any, error) {
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var v map[string]any
		if e := rows.Scan(&v); e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func parseCoverageCSV(ctx context.Context, cfg Config, origin, raw string, important bool) ([]inventoryRecord, error) {
	if len(raw) > 2<<20 {
		return nil, fmt.Errorf("розмір перевищує 2 МіБ")
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(raw, "\ufeff")))
	header, e := reader.Read()
	if e != nil {
		return nil, fmt.Errorf("потрібен CSV із колонкою Page, URL або Сторінка")
	}
	column := -1
	for i, h := range header {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "page", "url", "сторінка", "сторінки", "top pages", "популярні сторінки":
			if column >= 0 {
				return nil, fmt.Errorf("повторна колонка URL")
			}
			column = i
		}
	}
	if column < 0 {
		return nil, fmt.Errorf("відсутня колонка Page, URL або Сторінка")
	}
	result := []inventoryRecord{}
	seen := map[string]bool{}
	for line := 2; line <= 5002; line++ {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("некоректний CSV, рядок %d", line)
		}
		if line == 5002 {
			return nil, fmt.Errorf("не більше 5000 рядків; усічення не виконується")
		}
		u, o, err := normalizeSiteURL(strings.TrimSpace(row[column]), cfg.AllowPrivateTargets)
		if err != nil || o != origin {
			return nil, fmt.Errorf("рядок %d: URL має належати origin аудиту", line)
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		v := inventoryURL(cfg, u, 4, 0)
		v.Important = important
		result = append(result, v)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("CSV не містить URL")
	}
	return result, nil
}

func (s *webServer) importCoverageGSC(w http.ResponseWriter, r *http.Request) {
	id, origin, ok := s.coverageSite(w, r, true)
	if !ok {
		return
	}
	var input struct {
		ID        string `json:"id"`
		CSV       string `json:"csv"`
		Important bool   `json:"important"`
	}
	if !decodeWebJSON(w, r, 8<<20, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) {
		writeWebError(w, 400, "invalid_id", "Потрібен UUID імпорту")
		return
	}
	records, e := parseCoverageCSV(r.Context(), s.cfg, origin, input.CSV, input.Important)
	if e != nil {
		writeWebError(w, 400, "invalid_csv", e.Error())
		return
	}
	digest := fingerprintURL(s.cfg.TargetFingerprintKey, strconv.FormatBool(input.Important)+"\x00"+input.CSV)
	tx, e := s.pool.Begin(r.Context())
	if e != nil {
		webDBError(w, e)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(8247310038)`); e != nil {
		webDBError(w, e)
		return
	}
	if e = lockCoverageDestination(r.Context(), tx, id); e != nil {
		coverageMutationError(w, e)
		return
	}
	var existing string
	var old []byte
	var oldRun, source string
	e = tx.QueryRow(r.Context(), `SELECT id::TEXT,digest,run_id::TEXT,source FROM audit_crawl_source_imports WHERE id=$1 OR (run_id=$2 AND source='gsc' AND digest=$3) ORDER BY (id=$1) DESC LIMIT 1`, input.ID, id, digest).Scan(&existing, &old, &oldRun, &source)
	if e == nil {
		if oldRun != id || source != "gsc" || !bytes.Equal(old, digest) {
			writeWebError(w, 409, "import_conflict", "ID вже використано")
			return
		}
		writeWebJSON(w, 200, map[string]any{"id": existing, "reused": true})
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		webDBError(w, e)
		return
	}
	var imports int
	if e = tx.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_crawl_source_imports WHERE run_id=$1`, id).Scan(&imports); e != nil {
		webDBError(w, e)
		return
	}
	if imports >= 32 {
		writeWebError(w, 409, "source_limit", "Не більше 32 джерел на аудит")
		return
	}
	if _, e = tx.Exec(r.Context(), `INSERT INTO audit_crawl_source_imports(id,run_id,source,digest,row_count) VALUES($1,$2,'gsc',$3,$4)`, input.ID, id, digest, len(records)); e != nil {
		webDBError(w, e)
		return
	}
	if e = storeInventory(r.Context(), tx, id, records); e != nil {
		webDBError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 201, map[string]any{"id": input.ID, "rows": len(records), "reused": false})
}

func (s *webServer) attachCoverageBots(w http.ResponseWriter, r *http.Request) {
	id, origin, ok := s.coverageSite(w, r, true)
	if !ok {
		return
	}
	var input struct {
		ID     string `json:"id"`
		Import string `json:"import_id"`
	}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) || !runIDPattern.MatchString(input.Import) {
		writeWebError(w, 400, "invalid_id", "Потрібні UUID")
		return
	}
	tx, e := s.pool.Begin(r.Context())
	if e != nil {
		webDBError(w, e)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, e = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(8247310038)`); e != nil {
		webDBError(w, e)
		return
	}
	if e = lockCoverageDestination(r.Context(), tx, id); e != nil {
		coverageMutationError(w, e)
		return
	}
	var check []byte
	e = tx.QueryRow(r.Context(), `SELECT key_check FROM audit_bot_imports WHERE id=$1`, input.Import).Scan(&check)
	if e != nil {
		webDBError(w, e)
		return
	}
	if !bytes.Equal(check, fingerprintURL(s.cfg.TargetFingerprintKey, "bot-import:"+input.Import)) {
		writeWebError(w, 409, "key_mismatch", "Повторно імпортуйте журнал із поточним HMAC-ключем")
		return
	}
	digest := fingerprintURL(s.cfg.TargetFingerprintKey, "coverage-bots:"+input.Import)
	var existing string
	var oldRun string
	var old []byte
	e = tx.QueryRow(r.Context(), `SELECT id::TEXT,run_id::TEXT,digest FROM audit_crawl_source_imports WHERE id=$1 OR (run_id=$2 AND source='bots' AND digest=$3) ORDER BY (id=$1) DESC LIMIT 1`, input.ID, id, digest).Scan(&existing, &oldRun, &old)
	if e == nil {
		if oldRun != id || !bytes.Equal(old, digest) {
			writeWebError(w, 409, "import_conflict", "ID вже використано")
			return
		}
		writeWebJSON(w, 200, map[string]any{"id": existing, "reused": true})
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		webDBError(w, e)
		return
	}
	var imports int
	if e = tx.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_crawl_source_imports WHERE run_id=$1`, id).Scan(&imports); e != nil {
		webDBError(w, e)
		return
	}
	if imports >= 32 {
		writeWebError(w, 409, "source_limit", "Не більше 32 джерел на аудит")
		return
	}
	if _, e = tx.Exec(r.Context(), `INSERT INTO audit_crawl_source_imports(id,run_id,source,digest,bot_import_id,row_count) VALUES($1,$2,'bots',$3,$4,0)`, input.ID, id, digest, input.Import); e != nil {
		webDBError(w, e)
		return
	}
	after := 0
	count := 0
	for {
		rows, err := tx.Query(r.Context(), `SELECT ordinal,safe_url,coverage_fingerprint,url_shape FROM audit_bot_events WHERE import_id=$1 AND ordinal>$2 AND coverage_fingerprint IS NOT NULL ORDER BY ordinal LIMIT 100`, input.Import, after)
		if err != nil {
			webDBError(w, err)
			return
		}
		records := []inventoryRecord{}
		fps := [][]byte{}
		last := after
		for rows.Next() {
			var ordinal int
			var safe string
			var fp []byte
			var shape *crawler.URLShape
			if e = rows.Scan(&ordinal, &safe, &fp, &shape); e != nil {
				rows.Close()
				webDBError(w, e)
				return
			}
			last = ordinal
			_, o, err := normalizeSiteURL(safe, s.cfg.AllowPrivateTargets)
			if err != nil || o != origin {
				continue
			}
			v := inventoryRecord{Fingerprint: fp, URL: redactURL(safe), Sources: 8, Shape: crawler.DescribeURL(safe)}
			if shape != nil {
				v.Shape = *shape
			}
			records = append(records, v)
			fps = append(fps, fp)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			webDBError(w, e)
			return
		}
		if last == after {
			break
		}
		after = last
		if e = storeInventory(r.Context(), tx, id, records); e != nil {
			webDBError(w, e)
			return
		}
		count += len(records)
		if _, e = tx.Exec(r.Context(), `INSERT INTO audit_crawl_bot_evidence(source_id,fingerprint,visits,matched_range_visits,first_seen,last_seen)
			SELECT $1,coverage_fingerprint,COUNT(*),COUNT(*) FILTER(WHERE verification='matched_range'),MIN(observed_at),MAX(observed_at) FROM audit_bot_events
			WHERE import_id=$2 AND coverage_fingerprint=ANY($3::BYTEA[]) GROUP BY coverage_fingerprint ON CONFLICT DO NOTHING`, input.ID, input.Import, fps); e != nil {
			webDBError(w, e)
			return
		}
	}
	if count == 0 {
		writeWebError(w, 400, "domain_mismatch", "Журнал не містить URL цього origin")
		return
	}
	if _, e = tx.Exec(r.Context(), `UPDATE audit_crawl_source_imports SET row_count=$2 WHERE id=$1`, input.ID, count); e != nil {
		webDBError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		webDBError(w, e)
		return
	}
	writeWebJSON(w, 201, map[string]any{"id": input.ID, "rows": count, "reused": false})
}

func (s *webServer) coveragePriority(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.coverageSite(w, r, true)
	if !ok {
		return
	}
	var input struct {
		Fingerprint string `json:"fingerprint"`
		Important   bool   `json:"important"`
	}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	fp, e := hex.DecodeString(input.Fingerprint)
	if e != nil || len(fp) != 32 {
		writeWebError(w, 400, "invalid_fingerprint", "Некоректна ціль")
		return
	}
	tag, e := s.pool.Exec(r.Context(), `UPDATE audit_crawl_inventory SET important=$3 WHERE run_id=$1 AND fingerprint=$2`, id, fp, input.Important)
	if e != nil {
		webDBError(w, e)
		return
	}
	if tag.RowsAffected() == 0 {
		webDBError(w, pgx.ErrNoRows)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
