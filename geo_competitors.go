package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type competitorInput struct {
	URL   string `json:"url"`
	Brand string `json:"brand"`
}
type competitorSetInput struct {
	ID          string            `json:"id"`
	Competitors []competitorInput `json:"competitors"`
	MaxPages    int               `json:"max_pages"`
	FreshHours  int               `json:"fresh_hours"`
}
type preparedCompetitor struct {
	URL, Domain, Brand string
	Site               bool
}

func prepareCompetitors(input *competitorSetInput, own string, cfg Config) ([]preparedCompetitor, error) {
	if !runIDPattern.MatchString(input.ID) || len(input.Competitors) < 2 || len(input.Competitors) > 5 {
		return nil, geoInputError{"Додайте 2–5 конкурентних доменів або URL."}
	}
	if input.MaxPages == 0 {
		input.MaxPages = 25
	}
	if input.FreshHours == 0 {
		input.FreshHours = 24
	}
	if input.MaxPages < 1 || input.MaxPages > 50 || input.FreshHours < 1 || input.FreshHours > 168 {
		return nil, geoInputError{"Ліміт: 1–50 сторінок; свіжість: 1–168 годин."}
	}
	items := []preparedCompetitor{}
	seen := map[string]bool{}
	for i, c := range input.Competitors {
		raw := strings.TrimSpace(c.URL)
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		normalized, _, err := normalizeSiteURL(raw, cfg.AllowPrivateTargets)
		if err != nil {
			return nil, geoInputError{"Потрібен дозволений HTTP(S) URL конкурента."}
		}
		u, _ := url.Parse(normalized)
		if u.RawQuery != "" || u.ForceQuery {
			return nil, geoInputError{"Для конкурентів використовуйте URL без query-параметрів і секретів."}
		}
		domain, err := geo.NormalizeDomain(u.Hostname())
		if err != nil || domain == own || strings.HasSuffix(domain, "."+own) || strings.HasSuffix(own, "."+domain) {
			return nil, geoInputError{"Конкурент має бути окремим коректним доменом."}
		}
		q := geo.Input{Domain: domain, Brand: c.Brand, Queries: "перевірка"}
		if _, err = geo.Validate(&q); err != nil {
			return nil, geoInputError{err.Error()}
		}
		if seen[domain] {
			return nil, geoInputError{"Один домен можна додати лише один раз."}
		}
		seen[domain] = true
		input.Competitors[i] = competitorInput{URL: normalized, Brand: redactText(q.Brand)}
		items = append(items, preparedCompetitor{normalized, domain, redactText(q.Brand), u.Path == "/"})
	}
	return items, nil
}

func (s *webServer) storeCompetitorSet(ctx context.Context, parent string, input competitorSetInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310021)"); err != nil {
		return err
	}
	var own string
	if err = tx.QueryRow(ctx, "SELECT domain FROM geo_reports WHERE id=$1 FOR UPDATE", parent).Scan(&own); err != nil {
		return err
	}
	items, err := prepareCompetitors(&input, own, s.cfg)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(input)
	hash := fingerprintURL(s.cfg.TargetFingerprintKey, string(raw))
	var prior []byte
	var priorParent string
	err = tx.QueryRow(ctx, "SELECT request_hash,report_id::text FROM geo_competitor_sets WHERE id=$1", input.ID).Scan(&prior, &priorParent)
	if err == nil {
		if !bytes.Equal(prior, hash) || priorParent != parent {
			return geoInputError{"ID порівняння вже використаний для інших даних."}
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_sets WHERE report_id=$1", parent).Scan(&count); err != nil {
		return err
	}
	if count >= 20 {
		return geoInputError{"Дозволено до 20 порівнянь для одного звіту."}
	}
	if err = tx.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE status IN ('pending','scanning')").Scan(&count); err != nil {
		return err
	}
	if count+len(items) > 100 {
		return geoInputError{"Черга конкурентів обмежена 100 активними завданнями."}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO geo_competitor_sets(id,report_id,request_hash,max_pages,fresh_hours) VALUES($1,$2,$3,$4,$5)", input.ID, parent, hash, input.MaxPages, input.FreshHours); err != nil {
		return err
	}
	for i, c := range items {
		if _, err = tx.Exec(ctx, "INSERT INTO geo_competitor_jobs(set_id,ordinal,requested_url,domain,brand,site_mode,report_id) VALUES($1,$2,$3,$4,$5,$6,$7)", input.ID, i+1, c.URL, c.Domain, c.Brand, c.Site, newWebRunID()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Claim, source run та snapshot фіксуються разом; втрачена відповідь COMMIT не створює друге сканування.
func (m *AuditManager) startCompetitor(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(m.cfg)+effectiveDBWriteTimeout(m.cfg))
	defer cancel()
	stop := context.AfterFunc(m.root, cancel)
	defer stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.root.Err() != nil || m.active != "" {
		return "", errAuditBusy
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollbackSiteTransaction(tx, m.cfg)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310021)"); err != nil {
		return "", err
	}
	var setID, requested string
	var ordinal, maxPages, hours int
	var site bool
	err = tx.QueryRow(ctx, `SELECT j.set_id::text,j.ordinal,j.requested_url,j.site_mode,c.max_pages,c.fresh_hours FROM geo_competitor_jobs j
	 JOIN geo_competitor_sets c ON c.id=j.set_id WHERE j.status='pending' AND NOT c.canceled
	 ORDER BY c.created_at,j.set_id,j.ordinal LIMIT 1 FOR UPDATE OF j,c SKIP LOCKED`).Scan(&setID, &ordinal, &requested, &site, &maxPages, &hours)
	if err != nil {
		return "", err
	}
	// Повторно застосовуємо поточні правила URL, якщо процес перезапустився зі строгішою конфігурацією.
	normalized, _, err := normalizeSiteURL(requested, m.cfg.AllowPrivateTargets)
	if err != nil {
		if _, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET status='failed',error_message='URL заборонений поточною конфігурацією',finished_at=CURRENT_TIMESTAMP WHERE set_id=$1 AND ordinal=$2", setID, ordinal); err != nil {
			return "", err
		}
		return "", tx.Commit(ctx)
	}
	var reusable string
	err = tx.QueryRow(ctx, `SELECT r.id::text FROM audit_runs r WHERE r.status IN ('completed','completed_with_errors')
	 AND r.finished_at>=CURRENT_TIMESTAMP-make_interval(hours=>$3) AND r.successful_urls BETWEEN 1 AND 1000
	 AND EXISTS(SELECT 1 FROM audit_results a WHERE a.run_id=r.id AND a.scan_status='completed' AND a.status_code=200
	 AND a.target_fingerprint=$2 AND a.fingerprint_key_id=$4 AND (a.geo_signals->>'version')::int>=3)
	 AND (NOT $5 OR EXISTS(SELECT 1 FROM audit_site_crawls sc WHERE sc.run_id=r.id AND sc.root_safe_url=$1 AND sc.max_pages>=$6 AND sc.max_depth>=2 AND NOT sc.use_sitemaps))
	 ORDER BY r.finished_at DESC,r.id DESC LIMIT 1`, normalized, fingerprintURL(m.cfg.TargetFingerprintKey, normalized), hours, m.cfg.TargetFingerprintKeyID, site, maxPages).Scan(&reusable)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET run_id=$3,status='scanning',reused=TRUE WHERE set_id=$1 AND ordinal=$2", setID, ordinal, reusable)
		if err != nil {
			return "", err
		}
		return reusable, tx.Commit(ctx)
	}
	cfg := m.cfg
	cfg.RunID, cfg.OwnerGeneration, cfg.RenderJavaScript = newWebRunID(), 1, false
	if site {
		err = insertSiteAuditRun(ctx, tx, cfg, siteCrawlOptions{RootURL: normalized, MaxPages: maxPages, MaxDepth: 2, UseSitemaps: false})
	} else {
		err = insertExplicitAuditRun(ctx, tx, cfg, []string{normalized})
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET run_id=$3,status='scanning' WHERE set_id=$1 AND ordinal=$2", setID, ordinal, cfg.RunID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	m.launchLocked(cfg)
	return cfg.RunID, nil
}

func (s *webServer) processCompetitorReport(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310021)"); err != nil {
		return false, err
	}
	var setID, runID, reportID, parent, domain, brand, status string
	var ordinal int
	var canceled bool
	err = tx.QueryRow(ctx, `SELECT j.set_id::text,j.ordinal,j.run_id::text,j.report_id::text,c.report_id::text,j.domain,j.brand,r.status,c.canceled
	 FROM geo_competitor_jobs j JOIN geo_competitor_sets c ON c.id=j.set_id JOIN audit_runs r ON r.id=j.run_id
	 WHERE j.status='scanning' AND r.status<>'running' ORDER BY c.created_at,j.set_id,j.ordinal LIMIT 1 FOR UPDATE OF j,c SKIP LOCKED`).Scan(&setID, &ordinal, &runID, &reportID, &parent, &domain, &brand, &status, &canceled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if canceled || (status != auditRunStatusCompleted && status != auditRunStatusCompletedWithErrors) {
		state := "failed"
		if canceled {
			state = "canceled"
		}
		_, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET status=$3,error_message=$4,finished_at=CURRENT_TIMESTAMP WHERE set_id=$1 AND ordinal=$2", setID, ordinal, state, "SEO-аудит: "+status)
		if err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	var queries []string
	rows, err := tx.Query(ctx, "SELECT result->>'query' FROM geo_query_results WHERE report_id=$1 ORDER BY ordinal LIMIT 1000", parent)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var q string
		if err = rows.Scan(&q); err != nil {
			rows.Close()
			return false, err
		}
		queries = append(queries, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	_, err = s.createGEOInTransaction(ctx, tx, geo.Input{ID: reportID, SourceRunID: runID, Domain: domain, Brand: brand}, queries)
	if errors.Is(err, errGEOAnalysisBusy) {
		return false, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if ctx.Err() != nil {
			return false, err
		}
		var e geoInputError
		permanent := errors.As(err, &e)
		_, recordErr := s.pool.Exec(ctx, `UPDATE geo_competitor_jobs SET attempts=LEAST(attempts+1,3),status=CASE WHEN $4 OR attempts>=2 THEN 'failed' ELSE 'scanning' END,error_message=$3,finished_at=CASE WHEN $4 OR attempts>=2 THEN CURRENT_TIMESTAMP ELSE NULL END WHERE set_id=$1 AND ordinal=$2 AND status='scanning'`, setID, ordinal, storedGEOError(err), permanent)
		if recordErr != nil {
			return false, recordErr
		}
		return false, err
	}
	_, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET status='ready',error_message='',finished_at=CURRENT_TIMESTAMP WHERE set_id=$1 AND ordinal=$2", setID, ordinal)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *webServer) registerGEOCompetitors(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/geo/reports/{id}/competitors", s.geoCompetitorCreate)
	mux.HandleFunc("GET /api/geo/reports/{id}/competitors", s.geoCompetitorHistory)
	mux.HandleFunc("GET /api/geo/competitors/{id}", s.geoCompetitorGet)
	mux.HandleFunc("POST /api/geo/competitors/{id}/cancel", s.geoCompetitorCancel)
	mux.HandleFunc("POST /api/geo/competitors/{id}/retry", s.geoCompetitorRetry)
}
func (s *webServer) geoCompetitorCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var input competitorSetInput
	if !decodeWebJSON(w, r, 18000, &input) {
		return
	}
	if err := s.storeCompetitorSet(r.Context(), id, input); err != nil {
		var e geoInputError
		if errors.As(err, &e) {
			writeWebError(w, 400, "invalid_competitors", e.Error())
		} else {
			webDBError(w, err)
		}
		return
	}
	writeWebJSON(w, 202, map[string]string{"id": input.ID})
}
func (s *webServer) geoCompetitorHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('id',c.id,'at',c.created_at,'canceled',c.canceled,'total',COUNT(j.*),'ready',COUNT(*) FILTER(WHERE j.status='ready'),'failed',COUNT(*) FILTER(WHERE j.status='failed')) FROM geo_competitor_sets c JOIN geo_competitor_jobs j ON j.set_id=c.id WHERE c.report_id=$1 GROUP BY c.id ORDER BY c.created_at DESC,c.id LIMIT 20`, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var item map[string]any
		if err = rows.Scan(&item); err != nil {
			webDBError(w, err)
			return
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, out)
}

type competitorRow struct {
	Domain   string                  `json:"domain"`
	Brand    string                  `json:"brand"`
	URL      string                  `json:"url"`
	Status   string                  `json:"status"`
	Reused   bool                    `json:"reused"`
	RunID    *string                 `json:"run_id"`
	ReportID string                  `json:"report_id"`
	Error    string                  `json:"error"`
	Rows     []geoRow                `json:"rows"`
	Gaps     map[int][]geo.SignalGap `json:"gaps"`
}

func (s *webServer) geoCompetitorGet(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if r.URL.Query().Get("after") == "" {
		after, err = 0, nil
	}
	if err != nil || after < 0 || after > 1000 {
		writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var parent string
	var canceled bool
	if err = tx.QueryRow(ctx, "SELECT report_id::text,canceled FROM geo_competitor_sets WHERE id=$1", id).Scan(&parent, &canceled); err != nil {
		webDBError(w, err)
		return
	}
	items := []competitorRow{}
	rows, err := tx.Query(ctx, "SELECT domain,brand,requested_url,status,reused,run_id::text,report_id::text,error_message FROM geo_competitor_jobs WHERE set_id=$1 ORDER BY ordinal", id)
	if err != nil {
		webDBError(w, err)
		return
	}
	for rows.Next() {
		var item competitorRow
		if err = rows.Scan(&item.Domain, &item.Brand, &item.URL, &item.Status, &item.Reused, &item.RunID, &item.ReportID, &item.Error); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	effective, err := loadEffectiveGEORows(ctx, tx, parent)
	if err != nil {
		webDBError(w, err)
		return
	}
	own := []geoRow{}
	for _, row := range effective {
		if row.Ordinal > after && row.Ordinal <= after+50 {
			own = append(own, row)
		}
	}
	byOrdinal := map[int]geo.Result{}
	citations := map[int][]geoCitation{}
	for _, row := range own {
		byOrdinal[row.Ordinal] = row.Result
		citations[row.Ordinal] = row.Citations
	}
	for i := range items {
		items[i].Rows = []geoRow{}
		items[i].Gaps = map[int][]geo.SignalGap{}
		if items[i].Status == "ready" {
			items[i].Rows, err = loadGEORowRange(ctx, tx, items[i].ReportID, after)
			if err != nil {
				webDBError(w, err)
				return
			}
			for _, row := range items[i].Rows {
				if p, ok := byOrdinal[row.Ordinal]; ok && geo.QueryKey(p.Query) == geo.QueryKey(row.Result.Query) {
					items[i].Gaps[row.Ordinal] = geo.ComparePageSignals(p, row.Result)
					if p.TargetID != nil && row.Result.TargetID != nil {
						for _, a := range citations[row.Ordinal] {
							for _, b := range row.Citations {
								if a.Engine == b.Engine && a.Citation == "no" && b.Citation == "yes" {
									items[i].Gaps[row.Ordinal] = append(items[i].Gaps[row.Ordinal], geo.SignalGap{Signal: "Ручне AI-цитування · " + geoEngineLabels[b.Engine], Evidence: "У наших останніх ручних перевірках: конкурент цитувався, наша ціль — ні. Це різні спостереження, не доказ переваги в одній відповіді."})
								}
							}
						}
					}
				}
			}
		}
	}
	writeWebJSON(w, 200, map[string]any{"id": id, "report_id": parent, "canceled": canceled, "own": own, "competitors": items, "after": after, "next": after + len(own)})
}

func loadGEORowRange(ctx context.Context, tx pgx.Tx, id string, after int) ([]geoRow, error) {
	rows, err := tx.Query(ctx, `SELECT q.ordinal,q.result,COALESCE((SELECT jsonb_agg(jsonb_build_object('engine',c.engine,'citation',c.citation,'brand_mention',c.brand_mention,'checked_at',c.checked_at) ORDER BY c.engine) FROM geo_citation_checks c WHERE c.report_id=q.report_id AND c.ordinal=q.ordinal),'[]'::jsonb) FROM geo_query_results q WHERE q.report_id=$1 AND q.ordinal>$2 AND q.ordinal<=$2+50 ORDER BY q.ordinal`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []geoRow{}
	for rows.Next() {
		var item geoRow
		if err = rows.Scan(&item.Ordinal, &item.Result, &item.Citations); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *webServer) geoCompetitorCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var setID string
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310021)"); err != nil {
		webDBError(w, err)
		return
	}
	if err = tx.QueryRow(ctx, "UPDATE geo_competitor_sets SET canceled=TRUE WHERE id=$1 RETURNING id::text", id).Scan(&setID); err != nil {
		webDBError(w, err)
		return
	}
	rows, err := tx.Query(ctx, "SELECT run_id::text FROM geo_competitor_jobs WHERE set_id=$1 AND status='scanning' AND NOT reused", id)
	if err != nil {
		webDBError(w, err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var runID string
		if err = rows.Scan(&runID); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		ids = append(ids, runID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	if _, err = tx.Exec(ctx, "UPDATE geo_competitor_jobs SET status='canceled',finished_at=CURRENT_TIMESTAMP WHERE set_id=$1 AND status='pending'", id); err != nil {
		webDBError(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		webDBError(w, err)
		return
	}
	for _, runID := range ids {
		s.manager.cancelRun(runID)
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
func (s *webServer) geoCompetitorRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct {
		Ordinal int `json:"ordinal"`
	}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	if body.Ordinal < 1 || body.Ordinal > 5 {
		writeWebError(w, 400, "invalid_ordinal", "Оберіть конкурента")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310021)"); err != nil {
		webDBError(w, err)
		return
	}
	var active int
	if err = tx.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE status IN ('pending','scanning')").Scan(&active); err != nil {
		webDBError(w, err)
		return
	}
	if active >= 100 {
		writeWebError(w, 409, "queue_full", "Черга конкурентів заповнена")
		return
	}
	tag, err := tx.Exec(ctx, `WITH candidate AS (SELECT j.set_id,j.ordinal,r.status FROM geo_competitor_jobs j JOIN geo_competitor_sets c ON c.id=j.set_id LEFT JOIN audit_runs r ON r.id=j.run_id WHERE j.set_id=$1 AND j.ordinal=$2 AND j.status='failed' AND NOT c.canceled)
	 UPDATE geo_competitor_jobs j SET status=CASE WHEN c.status IN ('completed','completed_with_errors') THEN 'scanning' ELSE 'pending' END,
	 run_id=CASE WHEN c.status IN ('completed','completed_with_errors') THEN j.run_id ELSE NULL END,reused=CASE WHEN c.status IN ('completed','completed_with_errors') THEN j.reused ELSE FALSE END,attempts=0,error_message='',finished_at=NULL
	 FROM candidate c WHERE j.set_id=c.set_id AND j.ordinal=c.ordinal AND j.status='failed'`, id, body.Ordinal)
	if err != nil {
		webDBError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeWebError(w, 409, "not_retryable", "Невдале сканування не знайдено або порівняння скасоване")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
