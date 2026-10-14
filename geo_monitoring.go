package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/aicitation"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type geoProfileInput struct {
	ID              string             `json:"id"`
	ScheduleID      string             `json:"schedule_id"`
	Name            string             `json:"name"`
	Domain          string             `json:"domain"`
	Brand           string             `json:"brand"`
	Queries         string             `json:"queries"`
	Semantic        bool               `json:"semantic,omitempty"`
	BrandEntity     *aicitation.Entity `json:"brand_entity,omitempty"`
	DesiredAIPolicy map[string]string  `json:"desired_ai_policy,omitempty"`
}

func (s *webServer) storeGEOProfile(ctx context.Context, input geoProfileInput) error {
	input.Name = strings.TrimSpace(input.Name)
	if !runIDPattern.MatchString(input.ID) || !runIDPattern.MatchString(input.ScheduleID) || !utf8.ValidString(input.Name) || strings.ContainsRune(input.Name, 0) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 120 {
		return geoInputError{"Потрібні коректні ID та назва до 120 символів."}
	}
	q := geo.Input{Domain: input.Domain, Brand: input.Brand, Queries: input.Queries}
	queries, err := geo.Validate(&q)
	if err != nil {
		return geoInputError{err.Error()}
	}
	for i := range queries {
		queries[i] = sanitizeGEOTextForStorage(queries[i], 300)
	}
	q.Queries = strings.Join(queries, "\n")
	queries, err = geo.Validate(&q)
	if err != nil {
		return geoInputError{err.Error()}
	}
	input.Domain, input.Brand, input.Queries = q.Domain, redactText(q.Brand), strings.Join(queries, "\n")
	input.Name = redactText(input.Name)
	entity := defaultGEOEntity(input.Brand, input.Domain)
	if input.BrandEntity != nil {
		entity, err = validateGEOEntity(*input.BrandEntity, input.Brand, input.Domain)
		if err != nil {
			return geoInputError{err.Error()}
		}
		input.BrandEntity = &entity
	}
	desired, err := geo.NormalizeDesiredAIPolicy(input.DesiredAIPolicy)
	if err != nil {
		return geoInputError{err.Error()}
	}
	input.DesiredAIPolicy = desired
	entityJSON, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(input)
	hash := fingerprintURL(s.cfg.TargetFingerprintKey, string(payload))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8247310020)"); err != nil {
		return err
	}
	var previous []byte
	err = tx.QueryRow(ctx, "SELECT request_hash FROM geo_profiles WHERE id=$1", input.ID).Scan(&previous)
	if err == nil {
		if !bytes.Equal(previous, hash) {
			return geoInputError{"ID профілю вже використаний для інших даних."}
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var source scheduleSource
	if err = tx.QueryRow(ctx, "SELECT source FROM audit_schedules WHERE id=$1 FOR SHARE", input.ScheduleID).Scan(&source); err != nil {
		return err
	}
	if source.Adaptive {
		return geoInputError{"Для GEO-моніторингу потрібен повний SEO-розклад, а не часткові адаптивні перевірки."}
	}
	matched := source.Site != nil && geo.InDomain(source.Site.RootURL, input.Domain)
	for _, raw := range source.URLs {
		matched = matched || geo.InDomain(raw, input.Domain)
	}
	if !matched {
		return geoInputError{"Домен профілю має відповідати цілям SEO-розкладу."}
	}
	var total, perSchedule int
	if err = tx.QueryRow(ctx, "SELECT COUNT(*),COUNT(*) FILTER(WHERE schedule_id=$1) FROM geo_profiles", input.ScheduleID).Scan(&total, &perSchedule); err != nil {
		return err
	}
	if total >= 100 || perSchedule >= 10 {
		return geoInputError{"Дозволено 100 GEO-профілів загалом і 10 на розклад."}
	}
	_, err = tx.Exec(ctx, "INSERT INTO geo_profiles(id,schedule_id,name,domain,brand,queries,request_hash,semantic,brand_entity,desired_ai_policy) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", input.ID, input.ScheduleID, input.Name, input.Domain, input.Brand, queries, hash, input.Semantic, entityJSON, desiredJSON)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO geo_profile_runs(profile_id,run_id,report_id,scheduled_at,desired_ai_policy)
	 SELECT $1,sr.run_id,$2,sr.scheduled_at,p.desired_ai_policy FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
	 JOIN geo_profiles p ON p.id=$1
	 WHERE sr.schedule_id=$3 AND r.status IN ('completed','completed_with_errors') ORDER BY sr.scheduled_at DESC,sr.run_id DESC LIMIT 1`, input.ID, newWebRunID(), input.ScheduleID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *webServer) processGEOJob(ctx context.Context) (bool, error) {
	done, err := s.processGEOProfileJob(ctx)
	if done || err != nil {
		return done, err
	}
	return s.processCompetitorReport(ctx)
}

func (s *webServer) processGEOProfileJob(ctx context.Context) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return false, err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var profileID, runID, reportID, status, domain, brand string
	var queries []string
	var semanticEnabled bool
	err = tx.QueryRow(ctx, `SELECT j.profile_id::text,j.run_id::text,j.report_id::text,r.status,p.domain,p.brand,p.queries,p.semantic
	 FROM geo_profile_runs j JOIN geo_profiles p ON p.id=j.profile_id JOIN audit_runs r ON r.id=j.run_id
	 WHERE j.status='pending' AND r.status<>'running' AND NOT EXISTS(SELECT 1 FROM geo_profile_runs older
	 WHERE older.profile_id=j.profile_id AND older.status='pending' AND (older.scheduled_at,older.run_id)<(j.scheduled_at,j.run_id))
	 ORDER BY j.scheduled_at,j.profile_id,j.run_id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`).Scan(&profileID, &runID, &reportID, &status, &domain, &brand, &queries, &semanticEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != auditRunStatusCompleted && status != auditRunStatusCompletedWithErrors {
		_, err = tx.Exec(ctx, "UPDATE geo_profile_runs SET status='interrupted',finished_at=CURRENT_TIMESTAMP,error_message=$3 WHERE profile_id=$1 AND run_id=$2", profileID, runID, "SEO-аудит завершився зі статусом "+status)
		if err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	report, err := s.createGEOInTransaction(ctx, tx, geo.Input{ID: reportID, SourceRunID: runID, Domain: domain, Brand: brand, Semantic: semanticEnabled}, queries)
	if errors.Is(err, errGEOAnalysisBusy) {
		return false, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if ctx.Err() != nil {
			return false, err
		}
		var inputErr geoInputError
		permanent := errors.As(err, &inputErr)
		_, recordErr := s.pool.Exec(ctx, `UPDATE geo_profile_runs SET attempts=LEAST(attempts+1,3),
		 status=CASE WHEN $4 OR attempts>=2 THEN 'failed' ELSE 'pending' END,
		 finished_at=CASE WHEN $4 OR attempts>=2 THEN CURRENT_TIMESTAMP ELSE NULL END,error_message=$3
		 WHERE profile_id=$1 AND run_id=$2 AND status='pending'`, profileID, runID, storedGEOError(err), permanent)
		if recordErr != nil {
			return false, recordErr
		}
		return false, err
	}
	var baseline *string
	var previous string
	err = tx.QueryRow(ctx, `SELECT p.report_id::text FROM geo_profile_runs p JOIN geo_reports g ON g.id=p.report_id
	 JOIN geo_profile_runs c ON c.profile_id=$1 AND c.run_id=$2 WHERE p.profile_id=$1 AND p.status='ready'
	 AND g.model=$3 AND (p.scheduled_at,p.run_id)<(c.scheduled_at,c.run_id) ORDER BY p.scheduled_at DESC,p.run_id DESC LIMIT 1`, profileID, runID, report.Model).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil {
		baseline = &previous
		oldRows, err := loadGEORows(ctx, tx, previous)
		if err != nil {
			return false, err
		}
		newRows, err := loadGEORows(ctx, tx, reportID)
		if err != nil {
			return false, err
		}
		oldResults, newResults := geoResults(oldRows), geoResults(newRows)
		batch := &pgx.Batch{}
		for _, c := range geo.CompareResults(oldResults, newResults) {
			batch.Queue("INSERT INTO geo_profile_changes(profile_id,run_id,ordinal,kind,query,before_value,after_value) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING", profileID, runID, c.Ordinal, c.Kind, c.Query, c.Before, c.After)
		}
		if err = tx.SendBatch(ctx, batch).Close(); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE geo_profile_runs SET status='ready',baseline_report_id=$3,error_message='',finished_at=CURRENT_TIMESTAMP WHERE profile_id=$1 AND run_id=$2", profileID, runID, baseline)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func geoResults(rows []geoRow) []geo.Result {
	out := make([]geo.Result, len(rows))
	for i := range rows {
		out[i] = rows[i].Result
	}
	return out
}
func storedGEOError(err error) string {
	s, _, _ := limitStorageString(redactText(err.Error()), 1000)
	return s
}

func (s *webServer) registerGEOStrategy(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/geo/reports/{id}/topics", s.geoTopics)
	mux.HandleFunc("POST /api/geo/profiles", s.geoProfileCreate)
	mux.HandleFunc("GET /api/geo/profiles", s.geoProfileList)
	mux.HandleFunc("POST /api/geo/profiles/{id}/update", s.geoProfileUpdate)
	mux.HandleFunc("GET /api/geo/profiles/{id}/history", s.geoProfileHistory)
	mux.HandleFunc("GET /api/geo/profiles/{id}/timeline", s.geoProfileTimeline)
	mux.HandleFunc("GET /api/geo/profiles/{id}/changes", s.geoProfileChanges)
	mux.HandleFunc("GET /api/geo/profiles/{id}/observations", s.geoProfileObservationChanges)
	mux.HandleFunc("POST /api/geo/profiles/{id}/retry", s.geoProfileRetry)
	s.registerGEOCompetitors(mux)
	s.registerGEOOpportunities(mux)
}

func (s *webServer) geoTopics(w http.ResponseWriter, r *http.Request) {
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
	report, err := scanGEOReport(tx.QueryRow(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := loadEffectiveGEORows(r.Context(), tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	topics, err := geo.ClusterTopics(r.Context(), geoResults(rows))
	if err != nil {
		webDBError(w, err)
		return
	}
	model := geo.TopicModel
	if report.Semantic {
		model = geo.SemanticTopicModel
	}
	writeWebJSON(w, 200, map[string]any{"model": model, "topics": topics})
}

func (s *webServer) geoProfileCreate(w http.ResponseWriter, r *http.Request) {
	var input geoProfileInput
	if !decodeWebJSON(w, r, 600000, &input) {
		return
	}
	if err := s.storeGEOProfile(r.Context(), input); err != nil {
		var e geoInputError
		if errors.As(err, &e) {
			writeWebError(w, 400, "invalid_profile", e.Error())
		} else {
			webDBError(w, err)
		}
		return
	}
	writeWebJSON(w, 201, map[string]string{"id": input.ID})
}

func (s *webServer) geoProfileList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('id',p.id,'schedule_id',p.schedule_id,'name',p.name,'domain',p.domain,'brand',p.brand,'brand_entity',p.brand_entity,'desired_ai_policy',p.desired_ai_policy,'query_count',jsonb_array_length(p.queries),'enabled',p.enabled,'created_at',p.created_at,'schedule_name',s.name)
	 FROM geo_profiles p JOIN audit_schedules s ON s.id=p.schedule_id ORDER BY p.created_at DESC,p.id LIMIT 100`)
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

func (s *webServer) geoProfileUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled         *bool              `json:"enabled"`
		BrandEntity     *aicitation.Entity `json:"brand_entity"`
		DesiredAIPolicy map[string]string  `json:"desired_ai_policy"`
	}
	if !decodeWebJSON(w, r, 32768, &body) {
		return
	}
	if body.Enabled == nil && body.BrandEntity == nil && body.DesiredAIPolicy == nil {
		writeWebError(w, 400, "empty_update", "Немає змін для збереження")
		return
	}
	var entityJSON any
	if body.BrandEntity != nil {
		var name, domain string
		if err := s.pool.QueryRow(r.Context(), "SELECT brand,domain FROM geo_profiles WHERE id=$1", id).Scan(&name, &domain); err != nil {
			webDBError(w, err)
			return
		}
		entity, err := validateGEOEntity(*body.BrandEntity, name, domain)
		if err != nil {
			writeWebError(w, 400, "invalid_brand_entity", err.Error())
			return
		}
		encoded, err := json.Marshal(entity)
		if err != nil {
			webDBError(w, err)
			return
		}
		entityJSON = encoded
	}
	var desiredJSON any
	if body.DesiredAIPolicy != nil {
		desired, err := geo.NormalizeDesiredAIPolicy(body.DesiredAIPolicy)
		if err != nil {
			writeWebError(w, 400, "invalid_ai_policy", err.Error())
			return
		}
		encoded, err := json.Marshal(desired)
		if err != nil {
			webDBError(w, err)
			return
		}
		desiredJSON = encoded
	}
	tag, err := s.pool.Exec(r.Context(), "UPDATE geo_profiles SET enabled=COALESCE($2,enabled),brand_entity=COALESCE($3::jsonb,brand_entity),desired_ai_policy=COALESCE($4::jsonb,desired_ai_policy) WHERE id=$1", id, body.Enabled, entityJSON, desiredJSON)
	if err != nil {
		webDBError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		webDBError(w, pgx.ErrNoRows)
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}
func (s *webServer) geoProfileRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var body struct {
		RunID string `json:"run_id"`
	}
	if !decodeWebJSON(w, r, 1024, &body) {
		return
	}
	if !runIDPattern.MatchString(body.RunID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ID запуску")
		return
	}
	tag, err := s.pool.Exec(r.Context(), "UPDATE geo_profile_runs SET status='pending',attempts=0,error_message='',finished_at=NULL WHERE profile_id=$1 AND run_id=$2 AND status='failed'", id, body.RunID)
	if err != nil {
		webDBError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeWebError(w, 409, "not_failed", "Невдалий GEO-job не знайдено")
		return
	}
	writeWebJSON(w, 200, map[string]bool{"ok": true})
}

func (s *webServer) geoProfileHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('run_id',j.run_id,'report_id',CASE WHEN j.status='ready' THEN j.report_id ELSE NULL END,'scheduled_at',j.scheduled_at,'status',j.status,'error',j.error_message,'baseline_report_id',j.baseline_report_id,'changes',(SELECT COUNT(*) FROM geo_profile_changes c WHERE c.profile_id=j.profile_id AND c.run_id=j.run_id)) FROM geo_profile_runs j WHERE j.profile_id=$1 ORDER BY j.scheduled_at DESC,j.run_id DESC LIMIT 50`, id)
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

func (s *webServer) geoProfileTimeline(w http.ResponseWriter, r *http.Request) {
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
	rows, err := s.pool.Query(r.Context(), `WITH recent AS (SELECT report_id,scheduled_at FROM geo_profile_runs WHERE profile_id=$1 AND status='ready' ORDER BY scheduled_at DESC,run_id DESC LIMIT 10)
	 SELECT jsonb_build_object('report_id',j.report_id,'model',g.model,'at',j.scheduled_at,'ordinal',q.ordinal,'query',q.result->>'query','url',q.result->>'target_url','relevance',q.result->'matching'->'relevance','google_ai',q.result->'eligibility'->'google_ai'->>'status','chatgpt_search',q.result->'eligibility'->'chatgpt_search'->>'status')
	 FROM recent j JOIN geo_reports g ON g.id=j.report_id JOIN geo_query_results q ON q.report_id=j.report_id WHERE q.ordinal>$2 AND q.ordinal<=$2+50 ORDER BY q.ordinal,j.scheduled_at DESC`, id, after)
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
	writeWebJSON(w, 200, map[string]any{"points": items, "after": after})
}

func (s *webServer) geoProfileChanges(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	if !runIDPattern.MatchString(runID) {
		writeWebError(w, 400, "invalid_run", "Оберіть запуск профілю")
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
	var current string
	var previous *string
	if err = tx.QueryRow(ctx, "SELECT report_id::text,baseline_report_id::text FROM geo_profile_runs WHERE profile_id=$1 AND run_id=$2 AND status='ready'", id, runID).Scan(&current, &previous); err != nil {
		webDBError(w, err)
		return
	}
	rows, err := tx.Query(ctx, "SELECT ordinal,query,kind,before_value,after_value FROM geo_profile_changes WHERE profile_id=$1 AND run_id=$2 AND ordinal>$3 AND ordinal<=$3+50 ORDER BY ordinal,kind", id, runID, after)
	if err != nil {
		webDBError(w, err)
		return
	}
	items := []geo.Change{}
	for rows.Next() {
		var c geo.Change
		if err = rows.Scan(&c.Ordinal, &c.Query, &c.Kind, &c.Before, &c.After); err != nil {
			rows.Close()
			webDBError(w, err)
			return
		}
		items = append(items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		webDBError(w, err)
		return
	}
	if previous != nil {
		a, err := loadGEORowRange(ctx, tx, *previous, after)
		if err != nil {
			webDBError(w, err)
			return
		}
		b, err := loadGEORowRange(ctx, tx, current, after)
		if err != nil {
			webDBError(w, err)
			return
		}
		old := map[string]geoRow{}
		for _, row := range a {
			old[geo.QueryKey(row.Result.Query)] = row
		}
		for _, row := range b {
			p, ok := old[geo.QueryKey(row.Result.Query)]
			if !ok {
				continue
			}
			items = append(items, geo.CompareCitations(row.Ordinal, row.Result.Query, citationObservations(p.Citations), citationObservations(row.Citations))...)
		}
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "baseline_report_id": previous, "after": after})
}

func citationObservations(items []geoCitation) []geo.CitationObservation {
	out := make([]geo.CitationObservation, len(items))
	for i, c := range items {
		out[i] = geo.CitationObservation{Engine: c.Engine, Citation: c.Citation, BrandMention: c.BrandMention}
	}
	return out
}
