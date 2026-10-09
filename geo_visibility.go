package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	if err = s.bindVisibilityRecords(ctx, tx, reportID, sourceID, domain, records); err != nil {
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

func (s *webServer) bindVisibilityRecords(ctx context.Context, tx pgx.Tx, reportID, sourceID, domain string, records []geo.VisibilityRecord) error {
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
	targets := map[string]int64{}
	rows, err = tx.Query(ctx, `SELECT target_id,target_fingerprint FROM audit_results
		WHERE run_id=$1 AND fingerprint_key_id=$2 AND scan_status='completed' AND status_code=200
		ORDER BY target_id LIMIT $3`, sourceID, s.cfg.TargetFingerprintKeyID, geo.MaxPages+1)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var fingerprint []byte
		if err = rows.Scan(&id, &fingerprint); err != nil {
			rows.Close()
			return err
		}
		if len(targets) >= geo.MaxPages {
			rows.Close()
			return geoInputError{"Джерело перевищує ліміт 1000 успішних сторінок."}
		}
		key := string(fingerprint)
		if _, exists := targets[key]; exists {
			targets[key] = 0
		} else {
			targets[key] = id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
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
			normalized, err := normalizeTargetURL(r.RequestURL, true)
			if err != nil || !geo.InDomain(normalized, domain) {
				return geoInputError{fmt.Sprintf("URL у рядку %d не належить домену звіту або некоректний.", i+2)}
			}
			r.URL = redactURL(normalized)
			parsed, _ := url.Parse(normalized)
			authority, err := crawler.NormalizeAuthority(parsed)
			if err != nil {
				return geoInputError{fmt.Sprintf("Некоректний host у рядку %d.", i+2)}
			}
			parsed.Host = authority
			if parsed.Path == "" {
				parsed.Path = "/"
			}
			identityURL = parsed.String()
			if id := targets[string(fingerprintURL(s.cfg.TargetFingerprintKey, normalized))]; id != 0 {
				r.TargetID = &id
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
