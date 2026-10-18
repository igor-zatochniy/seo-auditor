package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

const maxSearchCSVBytes = 4 << 20

func searchQueryKey(query string) string {
	return cases.Fold().String(norm.NFKC.String(strings.Join(strings.Fields(query), " ")))
}

func searchCountry(raw string) (string, error) {
	if len(raw) != 2 && len(raw) != 3 {
		return "", fmt.Errorf("потрібен код країни alpha-2 або alpha-3")
	}
	r, err := language.ParseRegion(strings.ToUpper(raw))
	if err != nil || !r.IsCountry() {
		return "", fmt.Errorf("невідомий код країни")
	}
	return r.ISO3(), nil
}

func searchFilters(r *http.Request) (string, string, error) {
	country := strings.TrimSpace(r.URL.Query().Get("country"))
	device := strings.ToLower(r.URL.Query().Get("device"))
	var err error
	if country != "" {
		country, err = searchCountry(country)
		if err != nil {
			return "", "", err
		}
	}
	if device != "" && device != "desktop" && device != "mobile" && device != "tablet" {
		return "", "", fmt.Errorf("некоректний пристрій")
	}
	return country, device, nil
}

type searchCSVRow struct {
	Day, Query, QueryKey, URL, Country, Device string
	Fingerprint                                []byte
	Impressions, Clicks                        int64
	Position                                   *float64
}

func searchOrigin(raw string) (string, error) {
	normalized, err := normalizeTargetURL(raw, true)
	if err != nil {
		return "", fmt.Errorf("некоректний HTTP/HTTPS URL")
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("некоректний URL")
	}
	authority, err := crawler.NormalizeAuthority(u)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + authority, nil
}

func parseSearchCSV(ctx context.Context, cfg Config, origin, raw string) ([]searchCSVRow, error) {
	if len(raw) > maxSearchCSVBytes || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("потрібен UTF-8 CSV до 4 МіБ")
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(raw, "\ufeff")))
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("потрібен CSV із заголовком")
	}
	columns := map[string]int{}
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		if _, exists := columns[key]; exists {
			return nil, fmt.Errorf("повторна колонка %s", key)
		}
		columns[key] = i
	}
	for _, key := range []string{"date", "query", "page", "country", "device", "impressions", "clicks"} {
		if _, exists := columns[key]; !exists {
			return nil, fmt.Errorf("відсутня обов'язкова колонка %s", key)
		}
	}
	rows := []searchCSVRow{}
	seen := map[string]bool{}
	for line := 2; ; line++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		values, readErr := r.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil || len(rows) >= 10000 {
			return nil, fmt.Errorf("рядок %d: некоректний CSV або перевищено 10 000 рядків", line)
		}
		get := func(key string) string { return strings.TrimSpace(values[columns[key]]) }
		day, e := time.Parse("2006-01-02", get("date"))
		if e != nil || day.Year() < 2000 || day.Year() > 2100 {
			return nil, fmt.Errorf("рядок %d: Date має формат YYYY-MM-DD", line)
		}
		query := get("query")
		if query == "" || utf8.RuneCountInString(query) > 300 || strings.ContainsFunc(query, unicode.IsControl) {
			return nil, fmt.Errorf("рядок %d: Query має містити від 1 до 300 символів без керівних знаків", line)
		}
		query = sanitizeGEOTextForStorage(query, 300)
		if len(searchQueryKey(query)) > 1200 || len(get("page")) > 2048 {
			return nil, fmt.Errorf("рядок %d: перевищено ліміт нормалізованого запиту або URL", line)
		}
		u, e := normalizeTargetURL(get("page"), true)
		actualOrigin, originErr := searchOrigin(u)
		if e != nil || originErr != nil || actualOrigin != origin {
			return nil, fmt.Errorf("рядок %d: Page має належати обраному origin", line)
		}
		country, countryErr := searchCountry(get("country"))
		if countryErr != nil {
			return nil, fmt.Errorf("рядок %d: Country має бути дво- або трилітерним кодом", line)
		}
		device := strings.ToLower(get("device"))
		if device != "desktop" && device != "mobile" && device != "tablet" {
			return nil, fmt.Errorf("рядок %d: Device має бути desktop, mobile або tablet", line)
		}
		impressions, e1 := strconv.ParseInt(get("impressions"), 10, 64)
		clicks, e2 := strconv.ParseInt(get("clicks"), 10, 64)
		if e1 != nil || e2 != nil || impressions < 0 || impressions > 1e12 || clicks < 0 || clicks > impressions {
			return nil, fmt.Errorf("рядок %d: некоректні Impressions або Clicks", line)
		}
		var position *float64
		if _, exists := columns["position"]; exists && get("position") != "" {
			v, e := strconv.ParseFloat(get("position"), 64)
			if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > 10000 {
				return nil, fmt.Errorf("рядок %d: некоректна Position", line)
			}
			position = &v
		}
		fp := fingerprintURL(cfg.TargetFingerprintKey, u)
		key := day.Format("2006-01-02") + "\x00" + searchQueryKey(query) + "\x00" + country + "\x00" + device + "\x00" + hex.EncodeToString(fp)
		if seen[key] {
			return nil, fmt.Errorf("рядок %d: повторна комбінація Date/Query/Page/Country/Device", line)
		}
		seen[key] = true
		rows = append(rows, searchCSVRow{day.Format("2006-01-02"), query, searchQueryKey(query), redactURL(u), country, device, fp, impressions, clicks, position})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV не містить спостережень")
	}
	return rows, nil
}

func (s *webServer) registerSearchInsights(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/audits/{id}/search-imports", s.searchImport)
	mux.HandleFunc("GET /api/audits/{id}/search-imports", s.searchImports)
	mux.HandleFunc("DELETE /api/audits/{id}/search-imports/{importID}", s.searchImportDelete)
	mux.HandleFunc("GET /api/audits/{id}/landing-pages", s.landingPages)
	mux.HandleFunc("GET /api/audits/{id}/landing-history", s.landingHistory)
	mux.HandleFunc("GET /api/audits/{id}/structure", s.siteStructure)
}

func insightOrigins(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM audit_runs WHERE id=$1`, id).Scan(&status); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT substring(safe_url FROM '^https?://[^/?#]+') FROM audit_results WHERE run_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	origins := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var raw *string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if raw != nil {
			origin, e := searchOrigin(*raw)
			if e == nil && !seen[origin] {
				origins = append(origins, origin)
				seen[origin] = true
			}
		}
	}
	sort.Strings(origins)
	return origins, rows.Err()
}

func selectedInsightOrigin(origins []string, requested string) (string, error) {
	if requested == "" && len(origins) > 0 {
		return origins[0], nil
	}
	for _, origin := range origins {
		if origin == requested {
			return origin, nil
		}
	}
	return "", fmt.Errorf("оберіть origin, наявний у результатах цього аудиту")
}

func searchInputError(w http.ResponseWriter, err error) {
	var input geoInputError
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &input):
		writeWebError(w, 400, "invalid_input", input.Error())
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeWebError(w, 409, "observation_conflict", "Такі спостереження вже імпортовано. Для заміни видаліть попередній імпорт")
	default:
		coverageMutationError(w, err)
	}
}

func (s *webServer) searchImport(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "busy", "Інший імпорт або створення звіту ще виконується")
		return
	}
	var input struct {
		ID     string `json:"id"`
		Origin string `json:"origin"`
		CSV    string `json:"csv"`
	}
	if !decodeWebJSON(w, r, 6<<20, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор імпорту")
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if err = lockCoverageDestination(r.Context(), tx, id); err != nil {
		searchInputError(w, err)
		return
	}
	origins, err := insightOrigins(r.Context(), tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	origin, err := selectedInsightOrigin(origins, input.Origin)
	if err != nil {
		writeWebError(w, 400, "invalid_origin", err.Error())
		return
	}
	records, err := parseSearchCSV(r.Context(), s.cfg, origin, input.CSV)
	if err != nil {
		writeWebError(w, 400, "invalid_csv", err.Error())
		return
	}
	hash := fingerprintURL(s.cfg.TargetFingerprintKey, origin+"\x00"+input.CSV)
	var previous struct {
		ID, Run, Origin, Key string
		Hash                 []byte
	}
	err = tx.QueryRow(r.Context(), `SELECT id::text,run_id::text,origin,fingerprint_key_id,payload_hash FROM audit_search_imports WHERE id=$1 OR (run_id=$2 AND origin=$3 AND payload_hash=$4) ORDER BY (id=$1) DESC,id LIMIT 1`, input.ID, id, origin, hash).Scan(&previous.ID, &previous.Run, &previous.Origin, &previous.Key, &previous.Hash)
	if err == nil {
		if previous.Run != id || previous.Origin != origin || previous.Key != s.cfg.TargetFingerprintKeyID || !bytes.Equal(previous.Hash, hash) {
			writeWebError(w, 409, "id_conflict", "Ідентифікатор імпорту вже використано для інших даних")
			return
		}
		writeWebJSON(w, 200, map[string]any{"id": previous.ID, "rows": len(records), "reused": true})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	var count int
	var rotated bool
	keyCheck := fingerprintURL(s.cfg.TargetFingerprintKey, "search-insights:"+id)
	err = tx.QueryRow(r.Context(), `SELECT COUNT(*),COALESCE(bool_or(origin=$2 AND (fingerprint_key_id<>$3 OR key_check IS NULL OR key_check<>$4)),FALSE) FROM audit_search_imports WHERE run_id=$1`, id, origin, s.cfg.TargetFingerprintKeyID, keyCheck).Scan(&count, &rotated)
	if err != nil {
		webDBError(w, err)
		return
	}
	if count >= 100 || rotated {
		writeWebError(w, 409, "import_unavailable", "Досягнуто 100 імпортів або змінено HMAC-ключ; не можна змішувати несумісні ідентичності")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_search_imports(id,run_id,origin,payload_hash,fingerprint_key_id,row_count,key_check) VALUES($1,$2,$3,$4,$5,$6,$7)`, input.ID, id, origin, hash, s.cfg.TargetFingerprintKeyID, len(records), keyCheck)
	if err != nil {
		searchInputError(w, err)
		return
	}
	_, err = tx.CopyFrom(r.Context(), pgx.Identifier{"audit_search_rows"}, []string{"import_id", "run_id", "origin", "day", "query", "query_key", "safe_url", "fingerprint", "country", "device", "impressions", "clicks", "position"}, pgx.CopyFromSlice(len(records), func(i int) ([]any, error) {
		v := records[i]
		day, e := time.Parse("2006-01-02", v.Day)
		if e != nil {
			return nil, e
		}
		return []any{input.ID, id, origin, pgtype.Date{Time: day, Valid: true}, v.Query, v.QueryKey, v.URL, v.Fingerprint, v.Country, v.Device, v.Impressions, v.Clicks, v.Position}, nil
	}))
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		searchInputError(w, err)
		return
	}
	writeWebJSON(w, 201, map[string]any{"id": input.ID, "rows": len(records), "reused": false})
}

func (s *webServer) searchImports(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	origins, err := insightOrigins(r.Context(), tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT jsonb_build_object('id',i.id,'origin',i.origin,'rows',i.row_count,'created_at',i.created_at,'start',MIN(s.day),'end',MAX(s.day)) FROM audit_search_imports i JOIN audit_search_rows s ON s.import_id=i.id WHERE i.run_id=$1 GROUP BY i.id ORDER BY i.created_at DESC,i.id`, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	items, err := diagnosticRows(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	geoRows, err := tx.Query(r.Context(), `SELECT jsonb_build_object('id',id,'domain',domain,'created_at',created_at,'queries',query_count) FROM geo_reports WHERE source_run_id=$1 ORDER BY created_at DESC,id DESC LIMIT 201`, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	reports, err := diagnosticRows(geoRows)
	if err != nil {
		webDBError(w, err)
		return
	}
	limited := len(reports) > 200
	if limited {
		reports = reports[:200]
	}
	writeWebJSON(w, 200, map[string]any{"origins": origins, "items": items, "geo_reports": reports, "geo_reports_limited": limited})
}

func (s *webServer) searchImportDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	importID := r.PathValue("importID")
	if !runIDPattern.MatchString(importID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор імпорту")
		return
	}
	_, err := s.pool.Exec(r.Context(), `DELETE FROM audit_search_imports WHERE id=$1 AND run_id=$2`, importID, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"deleted": true})
}

func insightOffset(r *http.Request) (int, error) {
	if r.URL.Query().Get("offset") == "" {
		return 0, nil
	}
	n, e := strconv.Atoi(r.URL.Query().Get("offset"))
	if e != nil || n < 0 || n > 1000000 {
		return 0, fmt.Errorf("некоректна сторінка")
	}
	return n, nil
}

// A leader is defined by impressions, not by average position. Ties and zero
// impressions intentionally have no leader; missing dates are never zero-filled.
const landingSummarySQL = `WITH ranked AS (
 SELECT s.*,dense_rank() OVER(PARTITION BY query_key,country,device,day ORDER BY impressions DESC) rank,
 SUM(impressions) OVER(PARTITION BY query_key,country,device,day) total
 FROM audit_search_rows s WHERE run_id=$1 AND origin=$2 AND ($3='' OR country=$3) AND ($4='' OR device=$4)
), daily AS (
 SELECT query_key,country,device,day,MIN(query) query,MIN(safe_url) url,MIN(encode(fingerprint,'hex')) fingerprint,
 COUNT(*) leaders,MAX(impressions) impressions,MAX(total) total
 FROM ranked WHERE rank=1 GROUP BY query_key,country,device,day
), numbered AS (SELECT *,row_number() OVER(PARTITION BY query_key,country,device ORDER BY day DESC) n FROM daily)
 SELECT jsonb_build_object('query',a.query,'query_key',a.query_key,'country',a.country,'device',a.device,'day',a.day,
 'current_url',CASE WHEN a.leaders=1 AND a.total>0 THEN a.url END,
 'current_fingerprint',CASE WHEN a.leaders=1 AND a.total>0 THEN a.fingerprint END,
 'previous_day',b.day,'previous_url',CASE WHEN b.leaders=1 AND b.total>0 THEN b.url END,
 'changed',CASE WHEN a.leaders=1 AND b.leaders=1 AND a.total>0 AND b.total>0 THEN a.fingerprint<>b.fingerprint END,
 'impressions',a.total,'leader_share',CASE WHEN a.leaders=1 AND a.total>0 THEN a.impressions/a.total END,
 'tied',a.leaders>1 AND a.total>0)
 FROM numbered a LEFT JOIN numbered b ON b.query_key=a.query_key AND b.country=a.country AND b.device=a.device AND b.n=2
 WHERE a.n=1 ORDER BY a.query_key COLLATE "C",a.country,a.device LIMIT 51 OFFSET $5`

func (s *webServer) insightRead(w http.ResponseWriter, r *http.Request) (pgx.Tx, string, string, bool) {
	id, ok := webRunID(w, r)
	if !ok {
		return nil, "", "", false
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return nil, "", "", false
	}
	origins, err := insightOrigins(r.Context(), tx, id)
	if err != nil {
		rollbackSiteTransaction(tx, s.cfg)
		webDBError(w, err)
		return nil, "", "", false
	}
	origin, err := selectedInsightOrigin(origins, r.URL.Query().Get("origin"))
	if err != nil {
		rollbackSiteTransaction(tx, s.cfg)
		writeWebError(w, 400, "invalid_origin", err.Error())
		return nil, "", "", false
	}
	return tx, id, origin, true
}

func insightGEO(ctx context.Context, tx pgx.Tx, id, origin, requested string) (string, []geoRow, error) {
	u, _ := url.Parse(origin)
	var reportID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM geo_reports WHERE source_run_id=$1 AND domain=$2 AND ($3='' OR id::text=$3) ORDER BY created_at DESC,id DESC LIMIT 1`, id, u.Hostname(), requested).Scan(&reportID)
	if errors.Is(err, pgx.ErrNoRows) && requested == "" {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	items, err := loadEffectiveGEORows(ctx, tx, reportID)
	return reportID, items, err
}

func (s *webServer) landingPages(w http.ResponseWriter, r *http.Request) {
	country, device, filterErr := searchFilters(r)
	if filterErr != nil {
		writeWebError(w, 400, "invalid_segment", filterErr.Error())
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
	rows, err := tx.Query(r.Context(), landingSummarySQL, id, origin, country, device, offset)
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
	reportID, geoRows, err := insightGEO(r.Context(), tx, id, origin, r.URL.Query().Get("geo_report_id"))
	if err != nil {
		webDBError(w, err)
		return
	}
	expected := map[string]geo.Result{}
	for _, v := range geoRows {
		expected[searchQueryKey(v.Result.Query)] = v.Result
	}
	var importKey string
	var identityCompatible bool
	err = tx.QueryRow(r.Context(), `SELECT i.fingerprint_key_id,EXISTS(
 SELECT 1 FROM audit_search_rows v JOIN audit_results a ON a.run_id=v.run_id AND a.target_fingerprint=v.fingerprint AND a.fingerprint_key_id=i.fingerprint_key_id
 WHERE v.run_id=$1 AND v.origin=$2) FROM audit_search_imports i WHERE i.run_id=$1 AND i.origin=$2 LIMIT 1`, id, origin).Scan(&importKey, &identityCompatible)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	for _, v := range items {
		v["expected_url"] = nil
		v["target_mismatch"] = nil
		if target, exists := expected[v["query_key"].(string)]; exists && target.TargetID != nil {
			var safe string
			var fp []byte
			var key string
			if err = tx.QueryRow(r.Context(), `SELECT safe_url,target_fingerprint,fingerprint_key_id FROM audit_results WHERE run_id=$1 AND target_id=$2`, id, *target.TargetID).Scan(&safe, &fp, &key); err != nil {
				webDBError(w, err)
				return
			}
			v["expected_url"] = safe
			if current, exists := v["current_fingerprint"].(string); exists && identityCompatible && key == importKey {
				v["target_mismatch"] = current != hex.EncodeToString(fp)
			}
		}
		delete(v, "current_fingerprint")
	}
	writeWebJSON(w, 200, map[string]any{"origin": origin, "geo_report_id": reportID, "items": items, "more": more, "offset": offset, "identity_compatible": identityCompatible,
		"note": "Лідер визначається за показами серед імпортованих URL, не за найкращою позицією. Порівнюються останні два наявні дні окремо для країни та пристрою. Пропущені дні/URL невідомі, не нуль. Сигнали не доводять канібалізацію; частки показів URL не є унікальною часткою пошукових видач."})
}

func (s *webServer) landingHistory(w http.ResponseWriter, r *http.Request) {
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
	query := r.URL.Query().Get("query")
	country, device, filterErr := searchFilters(r)
	if filterErr != nil {
		writeWebError(w, 400, "invalid_segment", filterErr.Error())
		return
	}
	if query == "" || len(query) > 1200 || country == "" || device == "" {
		writeWebError(w, 400, "invalid_segment", "Потрібні запит, країна та пристрій")
		return
	}
	rows, err := tx.Query(r.Context(), `WITH data AS (SELECT s.*,SUM(impressions) OVER(PARTITION BY day) total FROM audit_search_rows s WHERE run_id=$1 AND origin=$2 AND query_key=$3 AND country=$4 AND device=$5)
 SELECT jsonb_build_object('day',day,'url',safe_url,'url_id',encode(fingerprint,'hex'),'impressions',impressions,'clicks',clicks,'position',position,'share',CASE WHEN total>0 THEN impressions/total END)
 FROM data ORDER BY day DESC,impressions DESC,fingerprint LIMIT 101 OFFSET $6`, id, origin, searchQueryKey(query), country, device, offset)
	if err != nil {
		webDBError(w, err)
		return
	}
	items, err := diagnosticRows(rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	more := len(items) > 100
	if more {
		items = items[:100]
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "more": more, "offset": offset})
}
