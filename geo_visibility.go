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
	url         string
	fingerprint string
	keyID       string
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
	byExact := map[string][]visibilityTargetCandidate{}
	byCanonical := map[string][]visibilityTargetCandidate{}
	targetRows, err := tx.Query(ctx, `SELECT target_id,safe_url,target_fingerprint,fingerprint_key_id,url_signals
 FROM audit_results WHERE run_id=$1 AND scan_status='completed' AND status_code IS NOT NULL ORDER BY target_id`, sourceID)
	if err != nil {
		return err
	}
	for targetRows.Next() {
		var candidate visibilityTargetCandidate
		var fingerprint, rawSignals []byte
		if err = targetRows.Scan(&candidate.id, &candidate.url, &fingerprint, &candidate.keyID, &rawSignals); err != nil {
			targetRows.Close()
			return err
		}
		candidate.fingerprint = hex.EncodeToString(fingerprint)
		key := candidate.keyID + "\x00" + candidate.fingerprint
		byExact[key] = append(byExact[key], candidate)
		var signals crawler.URLSignals
		if len(rawSignals) > 0 && json.Unmarshal(rawSignals, &signals) == nil && signals.HTML != nil && signals.HTML.Valid && !signals.HTML.Truncated {
			for _, fp := range signals.HTML.Fingerprints {
				if len(fp) != 64 {
					continue
				}
				byCanonical[candidate.keyID+"\x00"+fp] = append(byCanonical[candidate.keyID+"\x00"+fp], candidate)
			}
		}
	}
	err = targetRows.Err()
	targetRows.Close()
	if err != nil {
		return err
	}
	// GA4 exports contain a path rather than an absolute URL. Resolve it only
	// against origins present in this report's audit; keep the query string intact.
	origins := map[string]struct{}{}
	for _, candidates := range byExact {
		for _, candidate := range candidates {
			parsed, parseErr := url.Parse(candidate.url)
			if parseErr == nil && parsed.Scheme != "" && parsed.Host != "" {
				origins[parsed.Scheme+"://"+parsed.Host] = struct{}{}
			}
		}
	}
	seen := map[string]bool{}
	for i := range records {
		if err = ctx.Err(); err != nil {
			return err
		}
		r := &records[i]
		r.Query = sanitizeGEOTextForStorage(r.Query, 300)
		identityURL := ""
		if r.RequestURL != "" {
			normalizedURLs := []string{}
			parsedInput, parseErr := url.Parse(strings.TrimSpace(r.RequestURL))
			if parseErr != nil {
				return geoInputError{fmt.Sprintf("Некоректний URL у рядку %d.", i+2)}
			}
			if parsedInput.IsAbs() {
				normalized, normalizeErr := normalizeTargetURL(r.RequestURL, true)
				if normalizeErr != nil || !geo.InDomain(normalized, domain) {
					return geoInputError{fmt.Sprintf("URL у рядку %d не належить домену звіту або некоректний.", i+2)}
				}
				normalizedURLs = append(normalizedURLs, normalized)
			} else if source == "ga4" {
				if strings.HasPrefix(r.RequestURL, "//") || parsedInput.Opaque != "" || parsedInput.Path == "" || strings.HasPrefix(strings.ToLower(r.RequestURL), "javascript:") {
					return geoInputError{fmt.Sprintf("Некоректний Landing page у рядку %d.", i+2)}
				}
				for origin := range origins {
					base, _ := url.Parse(origin + "/")
					resolved := base.ResolveReference(parsedInput)
					if normalized, normalizeErr := normalizeTargetURL(resolved.String(), true); normalizeErr == nil && geo.InDomain(normalized, domain) {
						normalizedURLs = append(normalizedURLs, normalized)
					}
				}
				if len(normalizedURLs) == 0 {
					return geoInputError{fmt.Sprintf("Landing page у рядку %d неможливо зіставити з origin аудиту.", i+2)}
				}
			} else {
				return geoInputError{fmt.Sprintf("URL у рядку %d має бути абсолютним.", i+2)}
			}
			uniqueURLs := map[string]bool{}
			for _, normalized := range normalizedURLs {
				uniqueURLs[normalized] = true
			}
			normalizedURLs = normalizedURLs[:0]
			for normalized := range uniqueURLs {
				normalizedURLs = append(normalizedURLs, normalized)
			}
			sort.Strings(normalizedURLs)
			if len(normalizedURLs) == 1 {
				r.URL = redactURL(normalizedURLs[0])
			} else {
				r.URL = sanitizeGEOTextForStorage(redactURL(r.RequestURL), 2048)
			}
			exactMatches := []visibilityTargetCandidate{}
			for _, normalized := range normalizedURLs {
				parsed, _ := url.Parse(normalized)
				authority, normalizeErr := crawler.NormalizeAuthority(parsed)
				if normalizeErr != nil {
					return geoInputError{fmt.Sprintf("Некоректний host у рядку %d.", i+2)}
				}
				parsed.Host = authority
				if parsed.Path == "" {
					parsed.Path = "/"
				}
				identityURL = parsed.String()
				fp := hex.EncodeToString(fingerprintURL(s.cfg.TargetFingerprintKey, identityURL))
				key := s.cfg.TargetFingerprintKeyID + "\x00" + fp
				exactMatches = append(exactMatches, byExact[key]...)
			}
			canonicalMatches := []visibilityTargetCandidate{}
			if len(uniqueVisibilityCandidates(exactMatches)) == 0 {
				for _, normalized := range normalizedURLs {
					parsed, _ := url.Parse(normalized)
					authority, _ := crawler.NormalizeAuthority(parsed)
					parsed.Host = authority
					if parsed.Path == "" {
						parsed.Path = "/"
					}
					fp := hex.EncodeToString(fingerprintURL(s.cfg.TargetFingerprintKey, parsed.String()))
					canonicalMatches = append(canonicalMatches, byCanonical[s.cfg.TargetFingerprintKeyID+"\x00"+fp]...)
				}
			}
			candidate, match := chooseVisibilityMatch(exactMatches, canonicalMatches)
			switch match {
			case "exact", "canonical_match":
				setVisibilityMatch(r, candidate, match)
			case "ambiguous":
				r.URLMatch = match
			default:
				r.URLMatch = "unmatched"
			}
		}
		identity := geo.QueryKey(r.Query) + "\x00" + identityURL
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
