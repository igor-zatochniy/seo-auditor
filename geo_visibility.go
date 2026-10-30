package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type geoVisibilityImport struct {
	ID          string    `json:"id"`
	ReportID    string    `json:"report_id"`
	Source      string    `json:"source"`
	Engine      string    `json:"engine"`
	PeriodStart string    `json:"period_start"`
	PeriodEnd   string    `json:"period_end"`
	Country     string    `json:"country"`
	Device      string    `json:"device"`
	RowCount    int       `json:"row_count"`
	CreatedAt   time.Time `json:"created_at"`
}

const visibilityColumns = `id::text,report_id::text,source,engine,period_start::text,period_end::text,country,device,row_count,created_at`

type visibilityTargetCandidate struct {
	id          int64
	fingerprint string
	keyID       string
}

const visibilityLookupBatchSize = 64

type visibilityBinding struct {
	exact, canonical []visibilityTargetCandidate
	identity, first  string
	multiple         bool
}

type visibilityLookupRequest struct {
	record      int
	fingerprint string
}

func scanVisibilityImport(row pgx.Row) (geoVisibilityImport, error) {
	var v geoVisibilityImport
	err := row.Scan(&v.ID, &v.ReportID, &v.Source, &v.Engine, &v.PeriodStart, &v.PeriodEnd, &v.Country, &v.Device, &v.RowCount, &v.CreatedAt)
	return v, err
}

func (s *webServer) storeVisibilityImport(ctx context.Context, reportID string, input geo.VisibilityInput, records []geo.VisibilityRecord) (geoVisibilityImport, error) {
	var empty geoVisibilityImport
	payload := input
	payload.ID = ""
	encoded, err := json.Marshal(payload)
	if err != nil {
		return empty, err
	}
	hash := fingerprintURL(s.cfg.TargetFingerprintKey, string(encoded))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var domain, sourceID string
	// Lock серіалізує імпорти одного звіту та захищає ідемпотентність після втрати відповіді.
	if err = tx.QueryRow(ctx, "SELECT domain,source_run_id::text FROM geo_reports WHERE id=$1 FOR UPDATE", reportID).Scan(&domain, &sourceID); err != nil {
		return empty, err
	}
	var previousHash []byte
	err = tx.QueryRow(ctx, "SELECT payload_hash FROM geo_visibility_imports WHERE id=$1", input.ID).Scan(&previousHash)
	if err == nil {
		previous, err := scanVisibilityImport(tx.QueryRow(ctx, "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE id=$1", input.ID))
		if err != nil {
			return empty, err
		}
		if previous.ReportID != reportID || !bytes.Equal(previousHash, hash) {
			return empty, geoInputError{"Ідентифікатор імпорту вже використаний для інших даних."}
		}
		return previous, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	previous, err := scanVisibilityImport(tx.QueryRow(ctx, "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE report_id=$1 AND payload_hash=$2", reportID, hash))
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT COUNT(*) FROM geo_visibility_imports WHERE report_id=$1", reportID).Scan(&count); err != nil {
		return empty, err
	}
	if count >= 100 {
		return empty, geoInputError{"Звіт уже має 100 імпортів; видаліть непотрібний перед додаванням нового."}
	}
	if err = s.bindVisibilityRecords(ctx, tx, reportID, sourceID, domain, input.Source, records); err != nil {
		return empty, err
	}
	created, err := scanVisibilityImport(tx.QueryRow(ctx, `INSERT INTO geo_visibility_imports
		(id,report_id,payload_hash,source,engine,period_start,period_end,country,device,row_count)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7::date,$8,$9,$10) RETURNING `+visibilityColumns,
		input.ID, reportID, hash, input.Source, input.Engine, input.PeriodStart, input.PeriodEnd, input.Country, input.Device, len(records)))
	if err != nil {
		return empty, err
	}
	batch := &pgx.Batch{}
	for i, row := range records {
		batch.Queue("INSERT INTO geo_visibility_rows(import_id,ordinal,observation) VALUES ($1,$2,$3)", input.ID, i+1, row)
	}
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return created, nil
}

func (s *webServer) bindVisibilityRecords(ctx context.Context, tx pgx.Tx, reportID, sourceID, domain, source string, records []geo.VisibilityRecord) error {
	queries := map[string]struct {
		ordinal int
		target  *int64
	}{}
	rows, err := tx.Query(ctx, "SELECT ordinal,result FROM geo_query_results WHERE report_id=$1 ORDER BY ordinal", reportID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ordinal int
		var result geo.Result
		if err = rows.Scan(&ordinal, &result); err != nil {
			rows.Close()
			return err
		}
		queries[geo.QueryKey(result.Query)] = struct {
			ordinal int
			target  *int64
		}{ordinal, result.TargetID}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	bindings := make([]visibilityBinding, len(records))
	relative := make(map[int]*url.URL)
	requests := make([]visibilityLookupRequest, 0, visibilityLookupBatchSize)
	flush := func() error {
		if len(requests) == 0 {
			return nil
		}
		if err := s.lookupVisibilityBatch(ctx, tx, sourceID, requests, bindings); err != nil {
			return err
		}
		requests = requests[:0]
		return nil
	}
	addURL := func(i int, normalized string) error {
		parsed, parseErr := url.Parse(normalized)
		if parseErr != nil {
			return parseErr
		}
		authority, normalizeErr := crawler.NormalizeAuthority(parsed)
		if normalizeErr != nil {
			return geoInputError{fmt.Sprintf("Некоректний host у рядку %d.", i+2)}
		}
		parsed.Host = authority
		if parsed.Path == "" {
			parsed.Path = "/"
		}
		identity := parsed.String()
		binding := &bindings[i]
		if binding.first == "" {
			binding.first = normalized
		} else if binding.first != normalized {
			binding.multiple = true
		}
		if identity > binding.identity {
			binding.identity = identity
		}
		requests = append(requests, visibilityLookupRequest{i, hex.EncodeToString(fingerprintURL(s.cfg.TargetFingerprintKey, identity))})
		if len(requests) == visibilityLookupBatchSize {
			return flush()
		}
		return nil
	}
	for i := range records {
		if err = ctx.Err(); err != nil {
			return err
		}
		r := &records[i]
		r.Query = sanitizeGEOTextForStorage(r.Query, 300)
		if r.RequestURL != "" {
			parsedInput, parseErr := url.Parse(strings.TrimSpace(r.RequestURL))
			if parseErr != nil {
				return geoInputError{fmt.Sprintf("Некоректний URL у рядку %d.", i+2)}
			}
			if parsedInput.IsAbs() {
				normalized, normalizeErr := normalizeTargetURL(r.RequestURL, true)
				if normalizeErr != nil || !geo.InDomain(normalized, domain) {
					return geoInputError{fmt.Sprintf("URL у рядку %d не належить домену звіту або некоректний.", i+2)}
				}
				if err := addURL(i, normalized); err != nil {
					return err
				}
			} else if source == "ga4" {
				if strings.HasPrefix(r.RequestURL, "//") || parsedInput.Opaque != "" || parsedInput.Path == "" || strings.HasPrefix(strings.ToLower(r.RequestURL), "javascript:") {
					return geoInputError{fmt.Sprintf("Некоректний Landing page у рядку %d.", i+2)}
				}
				relative[i] = parsedInput
			} else {
				return geoInputError{fmt.Sprintf("URL у рядку %d має бути абсолютним.", i+2)}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if len(relative) > 0 {
		// A cursor keeps GA4 origin discovery bounded without truncating the corpus.
		if _, err := tx.Exec(ctx, `DECLARE visibility_origins NO SCROLL CURSOR FOR
 SELECT DISTINCT substring(safe_url FROM '^https?://[^/?#]+') AS origin
 FROM audit_results WHERE run_id=$1 AND scan_status='completed' AND status_code IS NOT NULL
 AND safe_url ~ '^https?://[^/?#]+' ORDER BY origin`, sourceID); err != nil {
			return err
		}
		for {
			origins, err := visibilityOriginBatch(ctx, tx)
			if err != nil {
				return err
			}
			if len(origins) == 0 {
				break
			}
			for _, origin := range origins {
				base, parseErr := url.Parse(origin + "/")
				if parseErr != nil {
					continue
				}
				for i, path := range relative {
					if err := ctx.Err(); err != nil {
						return err
					}
					normalized, normalizeErr := normalizeTargetURL(base.ResolveReference(path).String(), true)
					if normalizeErr == nil && geo.InDomain(normalized, domain) {
						if err := addURL(i, normalized); err != nil {
							return err
						}
					}
				}
			}
			if err := flush(); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "CLOSE visibility_origins"); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i := range records {
		r, binding := &records[i], &bindings[i]
		if r.RequestURL != "" {
			if binding.first == "" {
				return geoInputError{fmt.Sprintf("Landing page у рядку %d неможливо зіставити з origin аудиту.", i+2)}
			}
			if binding.multiple {
				r.URL = sanitizeGEOTextForStorage(redactURL(r.RequestURL), 2048)
			} else {
				r.URL = redactURL(binding.first)
			}
			candidate, match := chooseVisibilityMatch(binding.exact, binding.canonical)
			switch match {
			case "exact", "canonical_match":
				setVisibilityMatch(r, candidate, match)
			case "ambiguous":
				r.URLMatch = match
			default:
				r.URLMatch = "unmatched"
			}
		}
		identity := geo.QueryKey(r.Query) + "\x00" + binding.identity
		if seen[identity] {
			return geoInputError{"CSV містить повторний запит/URL після нормалізації."}
		}
		seen[identity] = true
		if q, exists := queries[geo.QueryKey(r.Query)]; r.Query != "" && exists {
			if r.RequestURL == "" || (q.target != nil && r.TargetID != nil && *q.target == *r.TargetID) {
				ordinal := q.ordinal
				r.ReportOrdinal = &ordinal
			}
		}
		r.RequestURL = ""
	}
	return nil
}

func visibilityOriginBatch(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, "FETCH FORWARD 64 FROM visibility_origins")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	origins := make([]string, 0, visibilityLookupBatchSize)
	for rows.Next() {
		var origin string
		if err := rows.Scan(&origin); err != nil {
			return nil, err
		}
		origins = append(origins, origin)
	}
	return origins, rows.Err()
}

func (s *webServer) lookupVisibilityBatch(ctx context.Context, tx pgx.Tx, sourceID string, requests []visibilityLookupRequest, bindings []visibilityBinding) error {
	keys := make([]string, 0, len(requests))
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if !seen[request.fingerprint] {
			keys = append(keys, request.fingerprint)
			seen[request.fingerprint] = true
		}
	}
	for _, canonical := range []bool{false, true} {
		if canonical {
			keys = keys[:0]
			clear(seen)
			for _, request := range requests {
				if len(bindings[request.record].exact) == 0 && !seen[request.fingerprint] {
					keys = append(keys, request.fingerprint)
					seen[request.fingerprint] = true
				}
			}
			if len(keys) == 0 {
				break
			}
		}
		predicate := "a.target_fingerprint=decode(k.fingerprint,'hex')"
		if canonical {
			predicate = `(a.url_signals->'html'->'fingerprints') @> jsonb_build_array(k.fingerprint)
 AND a.url_signals->'html'->'valid'='true'::jsonb
 AND (a.url_signals->'html'->'truncated' IS NULL OR a.url_signals->'html'->'truncated'='false'::jsonb)`
		}
		rows, err := tx.Query(ctx, `SELECT k.fingerprint,c.target_id,c.target_fingerprint,c.fingerprint_key_id
 FROM unnest($2::text[]) AS k(fingerprint)
 CROSS JOIN LATERAL (SELECT a.target_id,a.target_fingerprint,a.fingerprint_key_id FROM audit_results a
 WHERE a.run_id=$1 AND a.fingerprint_key_id=$3 AND a.scan_status='completed' AND a.status_code IS NOT NULL
 AND `+predicate+` LIMIT 2) c`, sourceID, keys, s.cfg.TargetFingerprintKeyID)
		if err != nil {
			return err
		}
		matches := make(map[string][]visibilityTargetCandidate, len(keys))
		for rows.Next() {
			var key string
			var fingerprint []byte
			var candidate visibilityTargetCandidate
			if err := rows.Scan(&key, &candidate.id, &fingerprint, &candidate.keyID); err != nil {
				rows.Close()
				return err
			}
			candidate.fingerprint = hex.EncodeToString(fingerprint)
			matches[key] = append(matches[key], candidate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, request := range requests {
			binding := &bindings[request.record]
			for _, candidate := range matches[request.fingerprint] {
				if canonical {
					binding.canonical = appendVisibilityCandidate(binding.canonical, candidate)
				} else {
					binding.exact = appendVisibilityCandidate(binding.exact, candidate)
				}
			}
		}
	}
	return nil
}

func appendVisibilityCandidate(items []visibilityTargetCandidate, candidate visibilityTargetCandidate) []visibilityTargetCandidate {
	for _, item := range items {
		if item.id == candidate.id {
			return items
		}
	}
	// Two distinct IDs prove ambiguity; retaining the rest cannot change the result.
	if len(items) < 2 {
		return append(items, candidate)
	}
	return items
}

func chooseVisibilityMatch(exact, canonical []visibilityTargetCandidate) (visibilityTargetCandidate, string) {
	exact = uniqueVisibilityCandidates(exact)
	if len(exact) > 1 {
		return visibilityTargetCandidate{}, "ambiguous"
	}
	if len(exact) == 1 {
		return exact[0], "exact"
	}
	canonical = uniqueVisibilityCandidates(canonical)
	if len(canonical) > 1 {
		return visibilityTargetCandidate{}, "ambiguous"
	}
	if len(canonical) == 1 {
		return canonical[0], "canonical_match"
	}
	return visibilityTargetCandidate{}, "unmatched"
}

func uniqueVisibilityCandidates(items []visibilityTargetCandidate) []visibilityTargetCandidate {
	byID := make(map[int64]visibilityTargetCandidate, len(items))
	for _, item := range items {
		byID[item.id] = item
	}
	out := make([]visibilityTargetCandidate, 0, len(byID))
	for _, item := range byID {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func setVisibilityMatch(record *geo.VisibilityRecord, candidate visibilityTargetCandidate, match string) {
	id := candidate.id
	record.TargetID = &id
	record.URLMatch = match
	record.MatchedFingerprint = candidate.fingerprint
	record.MatchKeyID = candidate.keyID
}

func (s *webServer) visibilitySubmit(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var input geo.VisibilityInput
	if !decodeWebJSON(w, r, 4<<20, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор імпорту")
		return
	}
	records, err := geo.ParseVisibilityCSV(&input)
	if err != nil {
		writeWebError(w, 400, "invalid_csv", err.Error())
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "busy", "Інше створення звіту ще виконується")
		return
	}
	result, err := s.storeVisibilityImport(r.Context(), id, input, records)
	if err != nil {
		var inputErr geoInputError
		if errors.As(err, &inputErr) {
			writeWebError(w, 400, "invalid_import", inputErr.Error())
		} else {
			webDBError(w, err)
		}
		return
	}
	writeWebJSON(w, 201, result)
}

func (s *webServer) visibilityHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE report_id=$1 ORDER BY created_at DESC,id LIMIT 100", id)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []geoVisibilityImport{}
	for rows.Next() {
		item, err := scanVisibilityImport(rows)
		if err != nil {
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

func (s *webServer) visibilityRows(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	importID := r.PathValue("importID")
	if !runIDPattern.MatchString(importID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор імпорту")
		return
	}
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.Atoi(raw)
		if err != nil || after < 0 || after > geo.MaxVisibilityRows {
			writeWebError(w, 400, "invalid_cursor", "Некоректний курсор")
			return
		}
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	item, err := scanVisibilityImport(tx.QueryRow(r.Context(), "SELECT "+visibilityColumns+" FROM geo_visibility_imports WHERE id=$1 AND report_id=$2", importID, id))
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), "SELECT ordinal,observation FROM geo_visibility_rows WHERE import_id=$1 AND ordinal>$2 ORDER BY ordinal LIMIT 101", importID, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []geo.VisibilityRecord{}
	next := 0
	last := 0
	for rows.Next() {
		var ordinal int
		var record geo.VisibilityRecord
		if err = rows.Scan(&ordinal, &record); err != nil {
			webDBError(w, err)
			return
		}
		if len(items) == 100 {
			next = last
			break
		}
		items = append(items, record)
		last = ordinal
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]any{"import": item, "rows": items, "next": next})
}

func (s *webServer) visibilityDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	importID := r.PathValue("importID")
	if !runIDPattern.MatchString(importID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор імпорту")
		return
	}
	if _, err := s.pool.Exec(r.Context(), "DELETE FROM geo_visibility_imports WHERE id=$1 AND report_id=$2", importID, id); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"deleted": true})
}
