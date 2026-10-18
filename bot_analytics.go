package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/botanalytics"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

func (s *webServer) registerBotAnalytics(mux *http.ServeMux) {
	page := template.Must(template.ParseFS(webAssets, "web/templates/bots.html"))
	mux.HandleFunc("GET /bots", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.ExecuteTemplate(w, "bots.html", nil)
	})
	mux.HandleFunc("GET /api/bots/networks", s.botNetworks)
	mux.HandleFunc("POST /api/bots/networks/refresh", s.refreshBotNetworks)
	mux.HandleFunc("POST /api/bots/imports", s.importBotLogs)
	mux.HandleFunc("GET /api/bots/imports", s.listBotImports)
	mux.HandleFunc("GET /api/bots/imports/{id}", s.botImportReport)
	mux.HandleFunc("POST /api/bots/fetch-diagnostics", s.aiFetchDiagnostics)
}

func (s *webServer) loadBotNetworks(ctx context.Context) (map[string]botanalytics.Ranges, error) {
	rows, err := s.pool.Query(ctx, `SELECT bot,prefixes,fetched_at FROM audit_bot_networks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]botanalytics.Ranges{}
	for rows.Next() {
		var id string
		var raw []string
		var ranges botanalytics.Ranges
		if err = rows.Scan(&id, &raw, &ranges.FetchedAt); err != nil {
			return nil, err
		}
		for _, value := range raw {
			p, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, err
			}
			ranges.Prefixes = append(ranges.Prefixes, p)
		}
		result[id] = ranges
	}
	return result, rows.Err()
}

func (s *webServer) botNetworks(w http.ResponseWriter, r *http.Request) {
	networks, err := s.loadBotNetworks(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	items := []map[string]any{}
	for _, p := range botanalytics.Providers {
		n := networks[p.ID]
		items = append(items, map[string]any{"bot": p.ID, "label": p.Label, "source": p.Source, "fetched_at": n.FetchedAt, "count": len(n.Prefixes), "fresh": len(n.Prefixes) > 0 && time.Since(n.FetchedAt) < botanalytics.RangeTTL})
	}
	writeWebJSON(w, 200, items)
}

func fetchBotNetworks(ctx context.Context, client *http.Client, provider botanalytics.Provider) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", provider.Source, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (512<<10)+1))
	if err != nil {
		return nil, err
	}
	return botanalytics.ParseRanges(raw)
}

func (s *webServer) refreshBotNetworks(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !decodeWebJSON(w, r, 1024, &input) {
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 429, "busy", "Інша операція вже виконується")
		return
	}
	cfg := s.cfg
	cfg.AllowPrivateTargets = false
	cfg.Workers = 1
	transport := newHTTPTransport(cfg, 5*time.Second)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	items := []map[string]any{}
	for _, provider := range botanalytics.Providers {
		prefixes, err := fetchBotNetworks(r.Context(), client, provider)
		if err == nil {
			raw := make([]string, 0, len(prefixes))
			for _, prefix := range prefixes {
				raw = append(raw, prefix.String())
			}
			_, err = s.pool.Exec(r.Context(), `INSERT INTO audit_bot_networks(bot,prefixes,fetched_at) VALUES($1,$2,CURRENT_TIMESTAMP) ON CONFLICT(bot) DO UPDATE SET prefixes=EXCLUDED.prefixes,fetched_at=EXCLUDED.fetched_at`, provider.ID, raw)
		}
		item := map[string]any{"bot": provider.ID, "ok": err == nil}
		if err != nil {
			item["error"] = "Не вдалося оновити офіційний список; попередній список не змінено"
		}
		items = append(items, item)
	}
	writeWebJSON(w, 200, items)
}

func (s *webServer) importBotLogs(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID     string `json:"id"`
		Domain string `json:"domain"`
		Format string `json:"format"`
		Log    string `json:"log"`
	}
	if !decodeWebJSON(w, r, 16<<20, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) || len(input.Log) > botanalytics.MaxBytes {
		writeWebError(w, 400, "invalid_import", "Потрібні ID та журнал до 8 МіБ")
		return
	}
	domain, err := geo.NormalizeDomain(input.Domain)
	if err != nil {
		writeWebError(w, 400, "invalid_domain", err.Error())
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 429, "busy", "Інша операція вже виконується")
		return
	}
	networks, err := s.loadBotNetworks(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	records, stats, err := botanalytics.Parse(r.Context(), strings.NewReader(input.Log), input.Format, domain, networks, time.Now())
	if err != nil {
		writeWebError(w, 400, "invalid_log", err.Error())
		return
	}
	mac := hmac.New(sha256.New, s.cfg.TargetFingerprintKey)
	_, _ = mac.Write([]byte(input.Format + "\x00" + domain + "\x00" + input.Log))
	digest := mac.Sum(nil)
	keyID := s.cfg.TargetFingerprintKeyID
	if keyID == "" {
		keyID = DefaultTargetFingerprintKeyID
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(8247310033)`); err != nil {
		webDBError(w, err)
		return
	}
	var existing string
	var existingDigest []byte
	err = tx.QueryRow(r.Context(), `SELECT id::TEXT,digest FROM audit_bot_imports WHERE id=$1 OR (domain=$2 AND digest=$3 AND fingerprint_key_id=$4) ORDER BY (id=$1) DESC LIMIT 1`, input.ID, domain, digest, keyID).Scan(&existing, &existingDigest)
	if err == nil {
		if !hmac.Equal(existingDigest, digest) {
			writeWebError(w, 409, "import_conflict", "Цей ID уже використано для іншого журналу")
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE audit_bot_imports SET key_check=$2 WHERE id=$1 AND key_check IS NULL`, existing, fingerprintURL(s.cfg.TargetFingerprintKey, "bot-import:"+existing)); err != nil {
			webDBError(w, err)
			return
		}
		if err = updateBotCoverageMetadata(r.Context(), tx, s.cfg, existing, records); err != nil {
			webDBError(w, err)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			webDBError(w, err)
			return
		}
		writeWebJSON(w, 200, map[string]any{"id": existing, "stats": stats, "reused": true})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	var total int
	if err = tx.QueryRow(r.Context(), `SELECT COUNT(*) FROM audit_bot_imports`).Scan(&total); err != nil {
		webDBError(w, err)
		return
	}
	if total >= 1000 {
		writeWebError(w, 409, "import_limit", "Досягнуто ліміт 1000 імпортів")
		return
	}
	evidence := map[string]time.Time{}
	for id, n := range networks {
		evidence[id] = n.FetchedAt
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_bot_imports(id,domain,digest,fingerprint_key_id,format,stats,range_evidence,key_check) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, input.ID, domain, digest, keyID, input.Format, stats, evidence, fingerprintURL(s.cfg.TargetFingerprintKey, "bot-import:"+input.ID)); err != nil {
		webDBError(w, err)
		return
	}
	_, err = tx.CopyFrom(r.Context(), pgx.Identifier{"audit_bot_events"}, []string{"import_id", "ordinal", "safe_url", "target_fingerprint", "bot", "verification", "http_status", "observed_at", "url_shape", "coverage_fingerprint", "origin_status", "cache_status", "event_fingerprint"}, pgx.CopyFromSlice(len(records), func(i int) ([]any, error) {
		record := records[i]
		safe, _, _ := limitStorageString(redactURL(record.URL), 2048)
		normalized, _, e := normalizeSiteURL(record.URL, false)
		var shape *crawler.URLShape
		var coverageFP []byte
		if e == nil {
			value := crawler.DescribeURL(normalized)
			shape = &value
			coverageFP = fingerprintURL(s.cfg.TargetFingerprintKey, normalized)
		}
		var eventFP []byte
		if record.EventID != "" {
			eventFP = fingerprintURL(s.cfg.TargetFingerprintKey, "cdn-event:"+input.Format+"\x00"+domain+"\x00"+record.EventID)
		}
		var cache *string
		if record.CacheStatus != "" {
			value := record.CacheStatus
			cache = &value
		}
		return []any{input.ID, i + 1, safe, fingerprintURL(s.cfg.TargetFingerprintKey, record.URL), record.Bot, record.Verification, record.Status, record.At, shape, coverageFP, record.OriginStatus, cache, eventFP}, nil
	}))
	if err != nil {
		webDBError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 201, map[string]any{"id": input.ID, "stats": stats, "reused": false})
}

func (s *webServer) listBotImports(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'domain',domain,'stats',stats,'created_at',created_at) FROM audit_bot_imports ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var item map[string]any
		if err = rows.Scan(&item); err != nil {
			webDBError(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, items)
}

func (s *webServer) botImportReport(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.Atoi(raw)
		if err != nil || after < 0 || after > 10000 {
			writeWebError(w, 400, "invalid_cursor", "Некоректна сторінка")
			return
		}
	}
	var summary map[string]any
	err := s.pool.QueryRow(r.Context(), `SELECT jsonb_build_object('id',id,'domain',domain,'format',format,'stats',stats,'range_evidence',range_evidence,'created_at',created_at) FROM audit_bot_imports WHERE id=$1`, id).Scan(&summary)
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT bot,verification,COUNT(*),COUNT(*) FILTER(WHERE http_status>=400) FROM audit_bot_events WHERE import_id=$1 GROUP BY bot,verification ORDER BY bot,verification`, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	totals := []map[string]any{}
	for rows.Next() {
		var bot, verification string
		var count, failures int
		if err = rows.Scan(&bot, &verification, &count, &failures); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		totals = append(totals, map[string]any{"bot": bot, "verification": verification, "visits": count, "errors": failures})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err = s.pool.Query(r.Context(), `SELECT MIN(ordinal),safe_url,bot,verification,COUNT(*),COUNT(*) FILTER(WHERE http_status>=400),MIN(observed_at),MAX(observed_at),ARRAY_AGG(DISTINCT http_status ORDER BY http_status),ARRAY_AGG(DISTINCT origin_status ORDER BY origin_status) FILTER(WHERE origin_status IS NOT NULL),ARRAY_AGG(DISTINCT cache_status ORDER BY cache_status) FILTER(WHERE cache_status IS NOT NULL) FROM audit_bot_events WHERE import_id=$1 GROUP BY target_fingerprint,safe_url,bot,verification HAVING MIN(ordinal)>$2 ORDER BY MIN(ordinal) LIMIT 101`, id, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	next := 0
	for rows.Next() {
		var ordinal, count, failures int
		var statuses []int
		var origins []int
		var caches []string
		var safe, bot, verification string
		var first, last time.Time
		if err = rows.Scan(&ordinal, &safe, &bot, &verification, &count, &failures, &first, &last, &statuses, &origins, &caches); err != nil {
			webDBError(w, err)
			return
		}
		if len(items) == 100 {
			next = items[99]["cursor"].(int)
			break
		}
		items = append(items, map[string]any{"cursor": ordinal, "safe_url": safe, "bot": bot, "verification": verification, "visits": count, "errors": failures, "first": first, "last": last, "status_codes": statuses, "origin_status_codes": origins, "cache_statuses": caches})
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"summary": summary, "totals": totals, "items": items, "next": next})
}
