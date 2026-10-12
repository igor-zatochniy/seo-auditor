package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/aicitation"
	"github.com/jackc/pgx/v5"
)

type aiAnswerInput struct {
	ID           string   `json:"id"`
	Engine       string   `json:"engine"`
	Answer       string   `json:"answer"`
	Sources      string   `json:"sources"`
	Confirmed    bool     `json:"confirmed"`
	Selected     []string `json:"selected"`
	BrandMention bool     `json:"brand_mention"`
}

func answerScope(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	id, ok := webRunID(w, r)
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || n < 1 || n > 1000 {
		writeWebError(w, 400, "invalid_ordinal", "Некоректний номер запиту")
		return "", 0, false
	}
	return id, n, true
}

func (s *webServer) analyzeAIAnswer(ctx context.Context, tx pgx.Tx, id string, ordinal int, input aiAnswerInput) (aicitation.Analysis, error) {
	var own aicitation.Brand
	if err := tx.QueryRow(ctx, `SELECT g.brand,g.domain FROM geo_reports g JOIN geo_query_results q ON q.report_id=g.id
		WHERE g.id=$1 AND q.ordinal=$2`, id, ordinal).Scan(&own.Name, &own.Domain); err != nil {
		return aicitation.Analysis{}, err
	}
	rows, err := tx.Query(ctx, `SELECT j.brand,j.domain FROM geo_competitor_jobs j WHERE j.set_id=(
		SELECT id FROM geo_competitor_sets WHERE report_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1) ORDER BY j.ordinal LIMIT 5`, id)
	if err != nil {
		return aicitation.Analysis{}, err
	}
	competitors := []aicitation.Brand{}
	for rows.Next() {
		var b aicitation.Brand
		if err = rows.Scan(&b.Name, &b.Domain); err != nil {
			rows.Close()
			return aicitation.Analysis{}, err
		}
		competitors = append(competitors, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return aicitation.Analysis{}, err
	}
	a, err := aicitation.Analyze(input.Answer, input.Sources, own, competitors)
	if err != nil {
		return a, geoInputError{err.Error()}
	}
	for i := range a.Citations {
		c := &a.Citations[i]
		c.ID = hex.EncodeToString(fingerprintURL(s.cfg.TargetFingerprintKey, "ai-citation:"+c.URL))
		c.URL = redactURL(c.URL)
	}
	return a, nil
}

func (s *webServer) geoAnswerPreview(w http.ResponseWriter, r *http.Request) {
	id, ordinal, ok := answerScope(w, r)
	if !ok {
		return
	}
	var input aiAnswerInput
	if !decodeWebJSON(w, r, 160000, &input) {
		return
	}
	tx, err := s.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	a, err := s.analyzeAIAnswer(r.Context(), tx, id, ordinal, input)
	if err != nil {
		answerWebError(w, err)
		return
	}
	writeWebJSON(w, 200, a)
}

func answerWebError(w http.ResponseWriter, err error) {
	var inputErr geoInputError
	if errors.As(err, &inputErr) {
		writeWebError(w, 400, "invalid_answer", inputErr.Error())
		return
	}
	webDBError(w, err)
}

func (s *webServer) geoAnswerSave(w http.ResponseWriter, r *http.Request) {
	id, ordinal, ok := answerScope(w, r)
	if !ok {
		return
	}
	var input aiAnswerInput
	if !decodeWebJSON(w, r, 165000, &input) {
		return
	}
	if !input.Confirmed || !runIDPattern.MatchString(input.ID) || len(input.Selected) > aicitation.MaxCandidates {
		writeWebError(w, 400, "confirmation_required", "Потрібні ID та явне підтвердження розпізнаних джерел")
		return
	}
	check := geoCitation{ID: input.ID, Engine: input.Engine, Citation: "unknown", BrandMention: "no"}
	if err := validateGEOCitation(&check); err != nil {
		writeWebError(w, 400, "invalid_engine", err.Error())
		return
	}
	sort.Strings(input.Selected)
	payload, _ := json.Marshal(struct {
		Report  string
		Ordinal int
		Input   aiAnswerInput
	}{id, ordinal, input})
	hash := fingerprintURL(s.cfg.TargetFingerprintKey, string(payload))
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	// Одночасні повтори одного підтвердження не створюють другу observation.
	if _, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,31))", input.ID); err != nil {
		webDBError(w, err)
		return
	}
	var previous []byte
	err = tx.QueryRow(r.Context(), "SELECT payload_hash FROM geo_ai_answers WHERE id=$1", input.ID).Scan(&previous)
	if err == nil {
		if !bytes.Equal(hash, previous) {
			writeWebError(w, 409, "answer_conflict", "ID вже використано для іншого аналізу")
			return
		}
		writeWebJSON(w, 200, map[string]string{"id": input.ID})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	a, err := s.analyzeAIAnswer(r.Context(), tx, id, ordinal, input)
	if err != nil {
		answerWebError(w, err)
		return
	}
	selected := map[string]bool{}
	for _, key := range input.Selected {
		if selected[key] {
			writeWebError(w, 400, "invalid_selection", "Джерело повторюється")
			return
		}
		selected[key] = true
	}
	for i := range a.Citations {
		c := &a.Citations[i]
		if selected[c.ID] {
			c.Approved = true
			delete(selected, c.ID)
			if c.Relation == "own" {
				check.Citation = "yes"
				check.EvidenceURL = c.URL
			}
		}
	}
	if len(selected) > 0 {
		writeWebError(w, 400, "invalid_selection", "Джерела змінилися; виконайте розпізнавання повторно")
		return
	}
	a.BrandMention = input.BrandMention
	if input.BrandMention {
		check.BrandMention = "yes"
	}
	check.Note = "Підтверджений локальний аналіз AI-відповіді · " + a.Model
	err = tx.QueryRow(r.Context(), `INSERT INTO geo_citation_observations(id,report_id,ordinal,engine,citation,brand_mention,evidence_url,note)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING checked_at,sequence`, input.ID, id, ordinal, input.Engine, check.Citation, check.BrandMention, check.EvidenceURL, check.Note).Scan(&check.CheckedAt, &check.Sequence)
	if err != nil {
		webDBError(w, err)
		return
	}
	answer := boundedAnswerText(input.Answer, 100000)
	sources := boundedAnswerText(input.Sources, 20000)
	if _, err = tx.Exec(r.Context(), "INSERT INTO geo_ai_answers(id,payload_hash,answer_text,sources_text,analysis) VALUES($1,$2,$3,$4,$5)", input.ID, hash, answer, sources, a); err != nil {
		webDBError(w, err)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO geo_citation_checks(report_id,ordinal,engine,citation,brand_mention,evidence_url,note,checked_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(report_id,ordinal,engine) DO UPDATE SET citation=EXCLUDED.citation,brand_mention=EXCLUDED.brand_mention,
		evidence_url=EXCLUDED.evidence_url,note=EXCLUDED.note,checked_at=EXCLUDED.checked_at WHERE geo_citation_checks.checked_at<=EXCLUDED.checked_at`, id, ordinal, input.Engine, check.Citation, check.BrandMention, check.EvidenceURL, check.Note, check.CheckedAt)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 201, map[string]string{"id": input.ID})
}

func boundedAnswerText(text string, limit int) string {
	text = redactText(text)
	if len(text) <= limit {
		return text
	}
	// Redaction може збільшити текст; обрізання не розриває UTF-8.
	for len(text) > limit {
		_, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
	}
	return text
}

func (s *webServer) geoAnswerHistory(w http.ResponseWriter, r *http.Request) {
	id, ordinal, ok := answerScope(w, r)
	if !ok {
		return
	}
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 0 {
			writeWebError(w, 400, "invalid_cursor", "Некоректний курсор")
			return
		}
	}
	type item struct {
		ID        string              `json:"id"`
		Sequence  int64               `json:"sequence"`
		Engine    string              `json:"engine"`
		Answer    string              `json:"answer"`
		Analysis  aicitation.Analysis `json:"analysis"`
		CreatedAt time.Time           `json:"created_at"`
	}
	rows, err := s.pool.Query(r.Context(), `SELECT a.id::text,o.sequence,o.engine,a.answer_text,a.analysis,a.created_at FROM geo_ai_answers a
		JOIN geo_citation_observations o USING(id) WHERE o.report_id=$1 AND o.ordinal=$2 AND ($3::bigint=0 OR o.sequence<$3)
		ORDER BY o.sequence DESC LIMIT 11`, id, ordinal, before)
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rows.Close()
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.ID, &i.Sequence, &i.Engine, &i.Answer, &i.Analysis, &i.CreatedAt); err != nil {
			webDBError(w, err)
			return
		}
		items = append(items, i)
	}
	if err = rows.Err(); err != nil {
		webDBError(w, err)
		return
	}
	next := int64(0)
	if len(items) > 10 {
		items = items[:10]
		next = items[9].Sequence
	}
	writeWebJSON(w, 200, map[string]any{"items": items, "next": next})
}
