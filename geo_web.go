package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type geoReport struct {
	ID             string    `json:"id"`
	SourceRunID    string    `json:"source_run_id"`
	Domain         string    `json:"domain"`
	Brand          string    `json:"brand"`
	Model          string    `json:"model"`
	QueryCount     int       `json:"query_count"`
	PageCount      int       `json:"page_count"`
	MatchedCount   int       `json:"matched_count"`
	AmbiguousCount int       `json:"ambiguous_count"`
	CreatedAt      time.Time `json:"created_at"`
}

const geoReportColumns = `id::text,source_run_id::text,domain,brand,model,query_count,page_count,matched_count,ambiguous_count,created_at`

func scanGEOReport(row pgx.Row) (geoReport, error) {
	var report geoReport
	err := row.Scan(&report.ID, &report.SourceRunID, &report.Domain, &report.Brand, &report.Model, &report.QueryCount, &report.PageCount, &report.MatchedCount, &report.AmbiguousCount, &report.CreatedAt)
	return report, err
}

type geoInputError struct{ message string }

func (e geoInputError) Error() string { return e.message }

func (s *webServer) createGEO(ctx context.Context, input geo.Input, queries []string) (geoReport, error) {
	var empty geoReport
	// Одна транзакція: стабільне джерело, звіт і всі рядки або повний rollback.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return empty, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var locked bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(8247310004)").Scan(&locked); err != nil {
		return empty, err
	}
	if !locked {
		return empty, geoInputError{"Інший GEO-аналіз ще виконується. Спробуйте пізніше."}
	}
	input.Queries = strings.Join(queries, "\n")
	payload, err := json.Marshal(input)
	if err != nil {
		return empty, err
	}
	fingerprint := sha256.Sum256(payload)
	var savedHash []byte
	err = tx.QueryRow(ctx, "SELECT request_hash FROM geo_reports WHERE id=$1", input.ID).Scan(&savedHash)
	if err == nil {
		if !bytes.Equal(savedHash, fingerprint[:]) {
			return empty, geoInputError{"Ідентифікатор уже використаний для іншого набору запитів."}
		}
		return scanGEOReport(tx.QueryRow(ctx, "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", input.ID))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	var sourceStatus string
	if err = tx.QueryRow(ctx, "SELECT status FROM audit_runs WHERE id=$1 FOR SHARE", input.SourceRunID).Scan(&sourceStatus); err != nil {
		return empty, err
	}
	if sourceStatus != "completed" && sourceStatus != "completed_with_errors" {
		return empty, geoInputError{"Оберіть завершений SEO-аудит."}
	}
	rows, err := tx.Query(ctx, `SELECT target_id,safe_url,COALESCE(title,''),COALESCE(description,''),COALESCE(h1,''),
		COALESCE(h1_count,0),COALESCE(external_links_count,0),COALESCE(word_count,0),COALESCE(meta_robots,''),COALESCE(x_robots_tag,''),
		COALESCE(canonical_url,''),COALESCE(is_self_canonical,FALSE),geo_signals
		FROM audit_results WHERE run_id=$1 AND scan_status='completed' AND status_code=200 ORDER BY target_id LIMIT $2`, input.SourceRunID, geo.MaxPages+1)
	if err != nil {
		return empty, err
	}
	pages := []geo.Page{}
	count := 0
	for rows.Next() {
		count++
		var page geo.Page
		if err = rows.Scan(&page.TargetID, &page.URL, &page.Title, &page.Description, &page.H1, &page.H1Count, &page.ExternalLinks, &page.WordCount,
			&page.MetaRobots, &page.XRobotsTag, &page.Canonical, &page.SelfCanonical, &page.Signals); err != nil {
			rows.Close()
			return empty, err
		}
		if geo.InDomain(page.URL, input.Domain) {
			pages = append(pages, page)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return empty, err
	}
	if count > geo.MaxPages {
		return empty, geoInputError{"Джерело має понад 1000 успішних сторінок. Створіть окремий обмежений SEO-аудит."}
	}
	if len(pages) == 0 {
		return empty, geoInputError{"У цьому аудиті немає успішних HTML-сторінок указаного домену."}
	}
	results, err := geo.Analyze(ctx, queries, pages)
	if err != nil {
		return empty, err
	}
	matched, ambiguous := 0, 0
	for _, result := range results {
		if result.TargetID != nil {
			matched++
		}
		if result.Ambiguous {
			ambiguous++
		}
	}
	report, err := scanGEOReport(tx.QueryRow(ctx, `INSERT INTO geo_reports
		(id,source_run_id,request_hash,domain,brand,model,query_count,page_count,matched_count,ambiguous_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+geoReportColumns,
		input.ID, input.SourceRunID, fingerprint[:], input.Domain, input.Brand, geo.Model, len(queries), len(pages), matched, ambiguous))
	if err != nil {
		return empty, err
	}
	batch := &pgx.Batch{}
	for i, result := range results {
		batch.Queue("INSERT INTO geo_query_results(report_id,ordinal,result) VALUES ($1,$2,$3)", input.ID, i+1, result)
	}
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return report, nil
}

func (s *webServer) geoSubmit(w http.ResponseWriter, r *http.Request) {
	var input geo.Input
	if !decodeWebJSON(w, r, 600000, &input) {
		return
	}
	if !runIDPattern.MatchString(input.ID) || !runIDPattern.MatchString(input.SourceRunID) {
		writeWebError(w, 400, "invalid_id", "Некоректний ідентифікатор аудиту")
		return
	}
	queries, err := geo.Validate(&input)
	if err != nil {
		writeWebError(w, 400, "invalid_input", err.Error())
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 409, "busy", "Створення іншого аудиту ще виконується")
		return
	}
	report, err := s.createGEO(r.Context(), input, queries)
	if err != nil {
		var inputErr geoInputError
		if errors.As(err, &inputErr) {
			writeWebError(w, 409, "invalid_source", inputErr.Error())
		} else {
			webDBError(w, err)
		}
		return
	}
	writeWebJSON(w, 201, report)
}

func (s *webServer) geoSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT id::text,status,total_urls,started_at FROM audit_runs
		WHERE status IN ('completed','completed_with_errors') ORDER BY started_at DESC,id LIMIT 100`)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	type source struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		Total     int       `json:"total"`
		StartedAt time.Time `json:"started_at"`
	}
	list := []source{}
	for rows.Next() {
		var item source
		if err = rows.Scan(&item.ID, &item.Status, &item.Total, &item.StartedAt); err != nil {
			webDBError(w, err)
			return
		}
		list = append(list, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, list)
}

func (s *webServer) geoHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports ORDER BY created_at DESC,id LIMIT 100")
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	list := []geoReport{}
	for rows.Next() {
		item, err := scanGEOReport(rows)
		if err != nil {
			webDBError(w, err)
			return
		}
		list = append(list, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, list)
}

type geoRow struct {
	Ordinal   int           `json:"ordinal"`
	Result    geo.Result    `json:"result"`
	Citations []geoCitation `json:"citations"`
}

func loadGEORows(ctx context.Context, tx pgx.Tx, id string) ([]geoRow, error) {
	rows, err := tx.Query(ctx, `SELECT q.ordinal,q.result,COALESCE((SELECT jsonb_agg(jsonb_build_object(
		'engine',c.engine,'citation',c.citation,'brand_mention',c.brand_mention,'evidence_url',c.evidence_url,'note',c.note,'checked_at',c.checked_at) ORDER BY c.engine)
		FROM geo_citation_checks c WHERE c.report_id=q.report_id AND c.ordinal=q.ordinal),'[]'::jsonb)
		FROM geo_query_results q WHERE q.report_id=$1 ORDER BY q.ordinal LIMIT 1000`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []geoRow{}
	for rows.Next() {
		var item geoRow
		if err = rows.Scan(&item.Ordinal, &item.Result, &item.Citations); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *webServer) geoGet(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	report, err := scanGEOReport(tx.QueryRow(r.Context(), "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	rows, err := loadGEORows(r.Context(), tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	if r.PathValue("format") == "csv" {
		writeGEOCSV(w, report, rows)
		return
	}
	writeWebJSON(w, 200, map[string]any{"report": report, "rows": rows})
}

type geoCitation struct {
	ID           string     `json:"id,omitempty"`
	Sequence     int64      `json:"sequence,omitempty"`
	Engine       string     `json:"engine"`
	Citation     string     `json:"citation"`
	BrandMention string     `json:"brand_mention"`
	EvidenceURL  string     `json:"evidence_url"`
	Note         string     `json:"note"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
}

func validateGEOCitation(c *geoCitation) error {
	if c.ID != "" && !runIDPattern.MatchString(c.ID) {
		return errors.New("некоректний ідентифікатор спостереження")
	}
	switch c.Engine {
	case "google_ai", "chatgpt", "gemini", "perplexity", "other":
	default:
		return errors.New("оберіть AI-систему")
	}
	for _, value := range []string{c.Citation, c.BrandMention} {
		if value != "unknown" && value != "yes" && value != "no" {
			return errors.New("некоректний результат ручної перевірки")
		}
	}
	if !utf8.ValidString(c.Note) || utf8.RuneCountInString(c.Note) > 2000 || strings.ContainsFunc(c.Note, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("нотатка: до 2000 символів")
	}
	if len(c.EvidenceURL) > 2048 {
		return errors.New("посилання надто довге")
	}
	if c.EvidenceURL != "" {
		u, err := url.Parse(c.EvidenceURL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("потрібне HTTP(S)-посилання без облікових даних")
		}
		c.EvidenceURL = redactURL(c.EvidenceURL)
	}
	c.Note = redactText(c.Note)
	c.CheckedAt = nil
	c.Sequence = 0
	return nil
}

func (s *webServer) geoSaveCitation(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || ordinal < 1 || ordinal > 1000 {
		writeWebError(w, 400, "invalid_ordinal", "Некоректний номер запиту")
		return
	}
	var check geoCitation
	if !decodeWebJSON(w, r, 16000, &check) {
		return
	}
	if err = validateGEOCitation(&check); err != nil {
		writeWebError(w, 400, "invalid_check", err.Error())
		return
	}
	if check.ID == "" {
		check.ID = newWebRunID()
	}
	err = s.pool.QueryRow(r.Context(), `WITH observation AS (
		INSERT INTO geo_citation_observations(id,report_id,ordinal,engine,citation,brand_mention,evidence_url,note)
		SELECT $8,$1,$2,$3,$4,$5,$6,$7 WHERE EXISTS(SELECT 1 FROM geo_query_results WHERE report_id=$1 AND ordinal=$2)
		ON CONFLICT(id) DO UPDATE SET id=EXCLUDED.id
		WHERE geo_citation_observations.report_id=EXCLUDED.report_id AND geo_citation_observations.ordinal=EXCLUDED.ordinal
		AND geo_citation_observations.engine=EXCLUDED.engine AND geo_citation_observations.citation=EXCLUDED.citation
		AND geo_citation_observations.brand_mention=EXCLUDED.brand_mention AND geo_citation_observations.evidence_url=EXCLUDED.evidence_url
		AND geo_citation_observations.note=EXCLUDED.note RETURNING *
	), latest AS (
		INSERT INTO geo_citation_checks(report_id,ordinal,engine,citation,brand_mention,evidence_url,note,checked_at)
		SELECT report_id,ordinal,engine,citation,brand_mention,evidence_url,note,checked_at FROM observation
		ON CONFLICT(report_id,ordinal,engine) DO UPDATE SET citation=EXCLUDED.citation,brand_mention=EXCLUDED.brand_mention,
		evidence_url=EXCLUDED.evidence_url,note=EXCLUDED.note,checked_at=EXCLUDED.checked_at
		WHERE geo_citation_checks.checked_at <= EXCLUDED.checked_at
	) SELECT checked_at,sequence FROM observation`,
		id, ordinal, check.Engine, check.Citation, check.BrandMention, check.EvidenceURL, check.Note, check.ID).Scan(&check.CheckedAt, &check.Sequence)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeWebError(w, 409, "observation_conflict", "Запит не існує або ID спостереження вже використано з іншим вмістом")
			return
		}
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, check)
}

var geoIntentLabels = map[string]string{"commercial": "Комерційне порівняння", "transactional": "Транзакційний", "informational": "Інформаційний", "unknown": "Не визначено"}
var geoLevelLabels = map[string]string{"strong": "Сильні сигнали", "medium": "Часткові сигнали", "weak": "Слабкі сигнали", "unknown": "Недостатньо даних"}
var geoEngineLabels = map[string]string{"google_ai": "Google AI Overviews", "chatgpt": "ChatGPT", "gemini": "Gemini", "perplexity": "Perplexity", "other": "Інша система"}
var geoObservationLabels = map[string]string{"yes": "Так", "no": "Не виявлено", "unknown": "Не перевірено"}

func writeGEOCSV(w http.ResponseWriter, report geoReport, rows []geoRow) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{"Запит", "Намір (евристика)", "Цільовий URL", "Збіг термінів, %", "Стара оцінка v1, %", "Оцінка контенту", "Прогалини", "Застереження", "Останні ручні AI-спостереження", "Категорії сигналів", "Googlebot: robots.txt", "OAI-SearchBot: robots.txt", "Google: індексація за директивами", "Google: snippet за директивами", "Джерело контенту", "Модель", "GPTBot: навчання", "HTTP-спостереження", "Лабораторні LCP/CLS", "Збіги абзаців 300–500 символів"})
	for _, row := range rows {
		r := row.Result
		score := ""
		if r.Readiness != nil {
			score = strconv.Itoa(*r.Readiness)
		}
		observations := []string{}
		for _, c := range row.Citations {
			checked := ""
			if c.CheckedAt != nil {
				checked = c.CheckedAt.UTC().Format(time.RFC3339)
			}
			observations = append(observations, fmt.Sprintf("%s: цитування — %s; бренд — %s; %s; %s; %s", geoEngineLabels[c.Engine], geoObservationLabels[c.Citation], geoObservationLabels[c.BrandMention], c.EvidenceURL, c.Note, checked))
		}
		cells := []string{r.Query, geoIntentLabels[r.Intent], r.TargetURL, strconv.Itoa(r.Coverage), score, geoLevelLabels[r.Level], strings.Join(r.Gaps, "\n"), strings.Join(r.Warnings, "\n"), strings.Join(observations, "\n")}
		categories := []string{}
		for _, c := range r.Categories {
			categories = append(categories, fmt.Sprintf("%s: %d / %d", c.Name, c.Passed, c.Total))
		}
		search := geo.SearchControls{}
		if r.Search != nil {
			search = *r.Search
		}
		cells = append(cells, strings.Join(categories, "\n"), geoRuleLabel(search.GooglebotRules), geoRuleLabel(search.OAISearchBotRules), geoRuleLabel(search.GoogleIndexing), geoRuleLabel(search.GoogleSnippet), r.ContentSource, report.Model)
		cells = append(cells, geoRuleLabel(search.GPTBotRules), structuredReportValue(r.HTTP), structuredReportValue(r.Performance), structuredReportValue(r.BlockMatches))
		for i := range cells {
			cells[i] = safeCSVCell(cells[i])
		}
		_ = writer.Write(cells)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		webDBError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="geo-`+report.ID+`.csv"`)
	_, _ = w.Write(append([]byte{0xef, 0xbb, 0xbf}, buffer.Bytes()...))
}

func geoRuleLabel(value string) string {
	switch value {
	case geo.Allowed:
		return "Дозволено правилами"
	case geo.Blocked:
		return "Заборонено"
	default:
		return "Не перевірено"
	}
}

func (s *webServer) registerGEO(mux *http.ServeMux) {
	page := template.Must(template.ParseFS(webAssets, "web/templates/geo.html"))
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, nil)
	}
	mux.HandleFunc("GET /geo", handler)
	mux.HandleFunc("GET /geo/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := webRunID(w, r); ok {
			handler(w, r)
		}
	})
	mux.HandleFunc("GET /api/geo/sources", s.geoSources)
	mux.HandleFunc("GET /api/geo/reports", s.geoHistory)
	mux.HandleFunc("POST /api/geo/reports", s.geoSubmit)
	mux.HandleFunc("GET /api/geo/reports/{id}", s.geoGet)
	mux.HandleFunc("GET /api/geo/reports/{id}/export/{format}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("format") != "csv" {
			writeWebError(w, 400, "invalid_format", "Доступний формат CSV")
			return
		}
		s.geoGet(w, r)
	})
	mux.HandleFunc("POST /api/geo/reports/{id}/queries/{ordinal}/citation", s.geoSaveCitation)
	mux.HandleFunc("GET /api/geo/reports/{id}/queries/{ordinal}/observations", s.geoObservations)
}

func (s *webServer) geoObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || ordinal < 1 || ordinal > 1000 {
		writeWebError(w, 400, "invalid_ordinal", "Некоректний номер запиту")
		return
	}
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			writeWebError(w, 400, "invalid_cursor", "Некоректний курсор історії")
			return
		}
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id::text,sequence,engine,citation,brand_mention,evidence_url,note,checked_at
		FROM geo_citation_observations WHERE report_id=$1 AND ordinal=$2 AND ($3::bigint=0 OR sequence<$3)
		ORDER BY sequence DESC LIMIT 51`, id, ordinal, before)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []geoCitation{}
	for rows.Next() {
		var item geoCitation
		if err = rows.Scan(&item.ID, &item.Sequence, &item.Engine, &item.Citation, &item.BrandMention, &item.EvidenceURL, &item.Note, &item.CheckedAt); err != nil {
			webDBError(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	next := int64(0)
	if len(items) > 50 {
		items = items[:50]
		next = items[49].Sequence
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next})
}
