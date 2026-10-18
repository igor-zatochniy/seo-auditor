package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

func (s *webServer) registerGEOOpportunities(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/geo/reports/{id}/opportunities", s.geoOpportunityGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/tasks.csv", s.geoOpportunityGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/tasks.md", s.geoOpportunityGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/tasks.json", s.geoOpportunityGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/targets", s.geoTargets)
	mux.HandleFunc("POST /api/geo/reports/{id}/queries/{ordinal}/target", s.geoTargetDecision)
	mux.HandleFunc("GET /api/geo/reports/{id}/queries/{ordinal}/target-history", s.geoTargetHistory)
	mux.HandleFunc("GET /api/geo/reports/{id}/impact", s.geoImpactGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/visibility-series", s.geoVisibilitySeries)
}

func loadEffectiveGEORows(ctx context.Context, tx pgx.Tx, id string) ([]geoRow, error) {
	items, err := loadGEORows(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON(ordinal) ordinal,sequence,selected_result,reason FROM geo_target_decisions WHERE report_id=$1 ORDER BY ordinal,sequence DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byOrdinal := map[int]int{}
	for i := range items {
		byOrdinal[items[i].Ordinal] = i
		items[i].MappingMode = "automatic"
	}
	for rows.Next() {
		var ordinal int
		var revision int64
		var selected *geo.Result
		var reason string
		if err = rows.Scan(&ordinal, &revision, &selected, &reason); err != nil {
			return nil, err
		}
		if i, ok := byOrdinal[ordinal]; ok {
			items[i].DecisionRevision, items[i].DecisionReason = revision, reason
			if selected != nil {
				original := items[i].Result
				items[i].AutomaticResult = &original
				items[i].Result = *selected
				items[i].MappingMode = "manual"
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	counts := map[int64]int{}
	for _, item := range items {
		if item.Result.TargetID != nil {
			counts[*item.Result.TargetID]++
		}
	}
	for i := range items {
		if items[i].Result.TargetID != nil {
			items[i].Result.SharedQueries = counts[*items[i].Result.TargetID]
		}
		confidence := geo.MatchingConfidence(items[i].Result)
		items[i].Confidence = &confidence
	}
	return items, nil
}

func loadOpportunityInput(ctx context.Context, tx pgx.Tx, report geoReport, rows []geoRow) (geo.PlanInput, error) {
	input := geo.PlanInput{Results: geoResults(rows), Edges: map[geo.LinkPair]bool{}}
	corpus, err := ensureGEOCorpus(ctx, tx, report.SourceRunID, report.Domain)
	if err != nil {
		return input, err
	}
	input.WalkFacts = func(visit func(geo.PageFacts) error) error { return walkOpportunityFacts(ctx, tx, report, visit) }
	err = tx.QueryRow(ctx, `SELECT graph_ready,NOT limit_reached AND sitemap_state NOT IN ('pending','partial') AND NOT EXISTS(SELECT 1 FROM audit_site_nodes WHERE run_id=$1 AND links_truncated) FROM audit_site_crawls WHERE run_id=$1`, report.SourceRunID).Scan(&input.GraphReady, &input.GraphFull)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return input, err
	}
	input.IndexedLinks = func(linkCtx context.Context) ([]geo.LinkSuggestion, bool, error) {
		return corpus.internalLinks(linkCtx, input)
	}
	competitors, err := tx.Query(ctx, `SELECT j.report_id::text FROM geo_competitor_jobs j JOIN geo_reports g ON g.id=j.report_id WHERE j.status='ready' AND g.model=$2 AND j.set_id=(SELECT id FROM geo_competitor_sets WHERE report_id=$1 AND NOT canceled ORDER BY created_at DESC,id DESC LIMIT 1) ORDER BY j.ordinal LIMIT 5`, report.ID, report.Model)
	if err != nil {
		return input, err
	}
	var ids []string
	for competitors.Next() {
		var id string
		if err = competitors.Scan(&id); err != nil {
			competitors.Close()
			return input, err
		}
		ids = append(ids, id)
	}
	err = competitors.Err()
	competitors.Close()
	if err != nil {
		return input, err
	}
	for _, id := range ids {
		other, err := loadGEORows(ctx, tx, id)
		if err != nil {
			return input, err
		}
		byQuery := map[string]geo.Result{}
		for _, r := range other {
			byQuery[geo.QueryKey(r.Result.Query)] = r.Result
		}
		for _, r := range rows {
			if c, ok := byQuery[geo.QueryKey(r.Result.Query)]; ok {
				for _, gap := range geo.ComparePageSignals(r.Result, c) {
					input.Competitors = append(input.Competitors, geo.CompetitorProof{Ordinal: r.Ordinal, URL: c.TargetURL, ReportID: id, Gap: gap})
				}
			}
		}
	}
	return input, nil
}

func (s *webServer) geoOpportunityGet(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "busy", "Інший локальний аналіз ще виконується.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), geoOperationTimeout)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	report, err := scanGEOReport(tx.QueryRow(ctx, "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := loadEffectiveGEORows(ctx, tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	input, err := loadOpportunityInput(ctx, tx, report, rows)
	if err != nil {
		webDBError(w, err)
		return
	}
	plan, err := geo.BuildOpportunityPlan(ctx, input)
	if err != nil {
		webDBError(w, err)
		return
	}
	var bundle *remediationPackage
	if strings.HasSuffix(r.URL.Path, "tasks.md") || strings.HasSuffix(r.URL.Path, "tasks.json") {
		value, e := buildRemediationPackage(ctx, tx, report, plan)
		if e != nil {
			webDBError(w, e)
			return
		}
		bundle = &value
	}
	if err = tx.Commit(ctx); err != nil {
		webDBError(w, err)
		return
	}
	if bundle != nil {
		format := "md"
		if strings.HasSuffix(r.URL.Path, ".json") {
			format = "json"
		}
		writeRemediationPackage(w, *bundle, format)
		return
	}
	if strings.HasSuffix(r.URL.Path, "tasks.csv") {
		writeOpportunityCSV(w, report, plan)
		return
	}
	topicModel := geo.TopicModel
	if report.Semantic {
		topicModel = geo.SemanticTopicModel
	}
	writeWebJSON(w, 200, map[string]any{"report": report, "plan": plan, "rows": rows, "generated_at": time.Now().UTC(), "matching_model": report.Model, "topic_model": topicModel})
}

func writeOpportunityCSV(w http.ResponseWriter, report geoReport, plan geo.OpportunityPlan) {
	var buf bytes.Buffer
	out := csv.NewWriter(&buf)
	_ = out.Write([]string{"ID завдання", "Пріоритет", "Тип", "Цільовий URL", "Кластери", "Запити", "Дія", "Впевненість", "Підстава", "Докази", "GEO-звіт", "Модель плану", "Модель зіставлення", "Перевірено сторінок", "Відображення обмежене", "Застереження"})
	limited := "Ні"
	if plan.Limited {
		limited = "Так"
	}
	for _, t := range plan.Tasks {
		evidence := []string{}
		for _, e := range t.Evidence {
			evidence = append(evidence, e.Source+" · "+e.URL+" · "+e.Detail)
		}
		cells := []string{t.ID, opportunityPriorityLabel(t.Priority), opportunityKindLabel(t.Kind), t.TargetURL, strings.Join(t.Clusters, "; "), strings.Join(t.Queries, "; "), t.Action, confidenceLabel(t.Confidence.Level), t.Confidence.Reason, strings.Join(evidence, "\n"), report.ID, plan.Model, report.Model, strconv.Itoa(plan.CheckedPages), limited, strings.Join(plan.Warnings, "; ")}
		for i := range cells {
			cells[i] = safeCSVCell(cells[i])
		}
		_ = out.Write(cells)
	}
	out.Flush()
	if out.Error() != nil {
		webDBError(w, out.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="geo-tasks-`+report.ID+`.csv"`)
	_, _ = w.Write(append([]byte{0xef, 0xbb, 0xbf}, buf.Bytes()...))
}
func opportunityPriorityLabel(s string) string {
	switch s {
	case "high":
		return "Високий"
	case "medium":
		return "Середній"
	default:
		return "Низький"
	}
}
func opportunityKindLabel(s string) string {
	switch s {
	case "technical":
		return "Технічний доступ"
	case "content":
		return "Контент"
	case "mapping":
		return "Вибір цілі"
	case "entity":
		return "Сутності"
	case "internal_link":
		return "Перелінковка"
	case "competitor_gap":
		return "Відмінність конкурента"
	default:
		return "Перевірка наміру"
	}
}
func confidenceLabel(s string) string {
	switch s {
	case "high":
		return "Висока"
	case "medium":
		return "Середня"
	case "low":
		return "Низька"
	default:
		return "Недостатньо даних"
	}
}

type targetDecisionInput struct {
	ID               string `json:"id"`
	TargetID         *int64 `json:"target_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (s *webServer) geoTargetDecision(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	var input targetDecisionInput
	if !decodeWebJSON(w, r, 4096, &input) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if err != nil || ordinal < 1 || ordinal > 1000 || !runIDPattern.MatchString(input.ID) || input.ExpectedRevision < 0 || (input.TargetID != nil && *input.TargetID == 0) || !utf8.ValidString(input.Reason) || utf8.RuneCountInString(input.Reason) < 1 || utf8.RuneCountInString(input.Reason) > 500 || strings.ContainsFunc(input.Reason, unicode.IsControl) {
		writeWebError(w, 400, "invalid_target", "Потрібні коректні ID, ревізія та пояснення до 500 символів.")
		return
	}
	input.Reason = sanitizeGEOTextForStorage(input.Reason, 500)
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "busy", "Інший локальний аналіз ще виконується.")
		return
	}
	payload, _ := json.Marshal(struct {
		Report  string
		Ordinal int
		Input   targetDecisionInput
	}{id, ordinal, input})
	hash := sha256.Sum256(payload)
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	report, err := scanGEOReport(tx.QueryRow(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	var saved []byte
	var revision int64
	err = tx.QueryRow(r.Context(), "SELECT payload_hash,sequence FROM geo_target_decisions WHERE id=$1", input.ID).Scan(&saved, &revision)
	if err == nil {
		if !bytes.Equal(saved, hash[:]) {
			writeWebError(w, 409, "decision_conflict", "ID рішення вже використано для інших даних.")
			return
		}
		writeWebJSON(w, 200, map[string]int64{"revision": revision})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	if report.Model != geo.Model && report.Model != geo.SemanticModel {
		writeWebError(w, 409, "model_changed", "Створіть новий GEO-звіт поточною моделлю перед ручним вибором цілі.")
		return
	}
	var original geo.Result
	if err = tx.QueryRow(r.Context(), "SELECT result FROM geo_query_results WHERE report_id=$1 AND ordinal=$2", id, ordinal).Scan(&original); err != nil {
		webDBError(w, err)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT COALESCE(MAX(sequence),0),COUNT(*) FROM geo_target_decisions WHERE report_id=$1 AND ordinal=$2", id, ordinal).Scan(&revision, &count); err != nil {
		webDBError(w, err)
		return
	}
	if revision != input.ExpectedRevision {
		writeWebError(w, 409, "stale_revision", "Рішення вже змінене. Оновіть звіт перед збереженням.")
		return
	}
	if count >= 100 {
		writeWebError(w, 409, "history_limit", "Для запиту вже збережено 100 рішень. Створіть новий GEO-звіт.")
		return
	}
	var selected *geo.Result
	if input.TargetID != nil {
		corpus, err := ensureGEOCorpus(r.Context(), tx, report.SourceRunID, report.Domain)
		if err != nil {
			webDBError(w, err)
			return
		}
		result, err := corpus.chosen(r.Context(), original.Query, *input.TargetID, report.Brand, report.Semantic)
		if err != nil {
			writeWebError(w, 400, "invalid_target", err.Error())
			return
		}
		selected = &result
	}
	err = tx.QueryRow(r.Context(), `INSERT INTO geo_target_decisions(id,report_id,ordinal,source_run_id,target_id,selected_result,reason,payload_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING sequence`, input.ID, id, ordinal, report.SourceRunID, input.TargetID, selected, input.Reason, hash[:]).Scan(&revision)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 201, map[string]int64{"revision": revision})
}

func (s *webServer) geoTargetHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || ordinal < 1 || ordinal > 1000 {
		writeWebError(w, 400, "invalid_ordinal", "Некоректний номер запиту")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT jsonb_build_object('revision',sequence,'target_id',target_id,'url',selected_result->>'target_url','reason',reason,'at',created_at) FROM geo_target_decisions WHERE report_id=$1 AND ordinal=$2 ORDER BY sequence DESC LIMIT 100`, id, ordinal)
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
