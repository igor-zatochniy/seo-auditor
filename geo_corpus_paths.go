package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

const geoFactsProjection = `a.target_id,a.safe_url,LEFT(COALESCE(a.title,''),1000),a.status_code,a.scan_status,a.error_code,n.internal_inlinks_count,COALESCE(n.links_truncated,FALSE)`

func scanGEOFact(row pgx.Row) (geo.PageFacts, error) {
	var f geo.PageFacts
	err := row.Scan(&f.TargetID, &f.URL, &f.Title, &f.Status, &f.ScanStatus, &f.ErrorCode, &f.Inlinks, &f.Truncated)
	return f, err
}

func walkOpportunityFacts(ctx context.Context, tx pgx.Tx, report geoReport, visit func(geo.PageFacts) error) error {
	after := int64(math.MinInt64)
	for {
		rows, err := tx.Query(ctx, `SELECT `+geoFactsProjection+` FROM audit_results a LEFT JOIN audit_site_nodes n ON n.run_id=a.run_id AND n.target_id=a.target_id
 WHERE a.run_id=$1 AND a.target_id>$2 ORDER BY a.target_id LIMIT 128`, report.SourceRunID, after)
		if err != nil {
			return err
		}
		batch := []geo.PageFacts{}
		for rows.Next() {
			f, e := scanGEOFact(rows)
			if e != nil {
				rows.Close()
				return e
			}
			batch = append(batch, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, f := range batch {
			after = f.TargetID
			if geo.InDomain(f.URL, report.Domain) {
				if err = visit(f); err != nil {
					return err
				}
			}
		}
	}
}

func loadGEOFact(ctx context.Context, tx pgx.Tx, runID string, id int64) (geo.PageFacts, error) {
	return scanGEOFact(tx.QueryRow(ctx, `SELECT `+geoFactsProjection+` FROM audit_results a LEFT JOIN audit_site_nodes n ON n.run_id=a.run_id AND n.target_id=a.target_id WHERE a.run_id=$1 AND a.target_id=$2`, runID, id))
}

func (c *postgresGEOCorpus) chosen(ctx context.Context, query string, target int64, brand string, semanticEnabled ...bool) (geo.Result, error) {
	q, err := geo.PrepareIndexedQuery(ctx, query, c.summary.Pages, c)
	if err != nil {
		return geo.Result{}, err
	}
	candidate, err := c.score(ctx, q, target)
	if errors.Is(err, pgx.ErrNoRows) {
		return geo.Result{}, geoInputError{"Ціль не є успішною HTML-сторінкою домену цього GEO-аудиту."}
	}
	if err != nil {
		return geo.Result{}, err
	}
	r := geo.ResultForChosenCandidate(query, candidate)
	if len(semanticEnabled) > 0 && semanticEnabled[0] {
		prepared, units, err := geo.PrepareSemanticQuery(ctx, query, c.summary.Pages, c)
		if err != nil {
			return geo.Result{}, err
		}
		scored, err := c.score(ctx, prepared, target)
		if err != nil {
			return geo.Result{}, err
		}
		combined := geo.CombineSemantic(query, []geo.RankedCandidate{candidate}, []geo.RankedCandidate{scored}, units, prepared.Limited)
		combined.Semantic.Selected = false
		combined.Semantic.Reason = "Ціль задано користувачем; словниковий та лексичний збіги показані окремо, без автоматичної заміни."
		r.Lexical, r.Semantic = combined.Lexical, combined.Semantic
	}
	r.Entities = geo.EvaluateEntities(brand, candidate.Page.Signals)
	return r, nil
}

func (c *postgresGEOCorpus) internalLinks(ctx context.Context, input geo.PlanInput) ([]geo.LinkSuggestion, bool, error) {
	out := []geo.LinkSuggestion{}
	seen := map[int64]bool{}
	for _, r := range input.Results {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if r.TargetID == nil || seen[*r.TargetID] {
			continue
		}
		target := *r.TargetID
		seen[target] = true
		fact, err := loadGEOFact(ctx, c.tx, c.runID, target)
		if err != nil {
			return nil, false, err
		}
		if fact.Inlinks == nil || *fact.Inlinks > 2 || r.Eligibility == nil || r.Eligibility.GoogleAI.Status == geo.Blocked || r.Eligibility.ChatGPTSearch.Status == geo.Blocked {
			continue
		}
		q, err := geo.PrepareIndexedQuery(ctx, r.Query, c.summary.Pages, c)
		if r.Semantic != nil && r.Semantic.Selected {
			q, _, err = geo.PrepareSemanticQuery(ctx, r.Query, c.summary.Pages, c)
		}
		if err != nil {
			return nil, false, err
		}
		if q.TotalIDF == 0 {
			continue
		}
		data, err := json.Marshal(q.Variants)
		if err != nil {
			return nil, false, err
		}
		keys := q.PhraseKeys
		if keys == nil {
			keys = []string{}
		}
		lastRelevance, lastWeighted, lastID := 101, 101, int64(math.MinInt64)
		added := 0
		for added < 3 {
			rows, err := c.tx.Query(ctx, geoCandidateScoresSQL+`SELECT s.target_id,s.relevance,s.weighted FROM ranked s
 WHERE s.target_id<>$7 AND (s.relevance<$8 OR s.relevance=$8 AND (s.weighted<$9 OR s.weighted=$9 AND s.target_id>$10))
 AND NOT EXISTS(SELECT 1 FROM audit_site_edges e JOIN audit_site_nodes n ON n.run_id=e.run_id AND n.url_fingerprint=e.to_fingerprint
 WHERE e.run_id=$1 AND e.from_target_id=s.target_id AND n.target_id=$7 AND e.is_internal)
 ORDER BY s.relevance DESC,s.weighted DESC,s.target_id LIMIT 32`, c.runID, c.domain, geoIndexModel, data, q.TotalIDF, keys, target, lastRelevance, lastWeighted, lastID)
			if err != nil {
				return nil, false, err
			}
			type item struct {
				id                  int64
				relevance, weighted int
			}
			batch := []item{}
			for rows.Next() {
				var v item
				if err = rows.Scan(&v.id, &v.relevance, &v.weighted); err != nil {
					rows.Close()
					return nil, false, err
				}
				batch = append(batch, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, false, err
			}
			if len(batch) == 0 {
				break
			}
			for _, v := range batch {
				lastID, lastRelevance, lastWeighted = v.id, v.relevance, v.weighted
				if v.weighted < 50 {
					continue
				}
				candidate, err := c.score(ctx, q, v.id)
				if err != nil {
					return nil, false, err
				}
				from, err := loadGEOFact(ctx, c.tx, c.runID, v.id)
				if err != nil {
					return nil, false, err
				}
				if l := geo.SuggestInternalLink(r, candidate, fact, from, input.GraphFull); l != nil {
					out = append(out, *l)
					added++
					if len(out) > 200 {
						return out[:200], true, nil
					}
					if added == 3 {
						break
					}
				}
			}
		}
	}
	return out, false, nil
}

func (s *webServer) geoTargets(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	after := int64(math.MinInt64)
	var err error
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if err != nil || len(search) > 512 {
		writeWebError(w, 400, "invalid_search", "Некоректний курсор або пошуковий рядок.")
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
	items := []geo.PageFacts{}
	for len(items) < 51 {
		rows, e := tx.Query(r.Context(), `SELECT `+geoFactsProjection+` FROM audit_results a LEFT JOIN audit_site_nodes n ON n.run_id=a.run_id AND n.target_id=a.target_id
 WHERE a.run_id=$1 AND a.target_id>$2 AND a.scan_status='completed' AND a.status_code=200
 AND ($3='' OR strpos(lower(a.safe_url||' '||COALESCE(a.title,'')),lower($3))>0) ORDER BY a.target_id LIMIT 128`, report.SourceRunID, after, search)
		if e != nil {
			webDBError(w, e)
			return
		}
		count := 0
		for rows.Next() {
			f, e := scanGEOFact(rows)
			if e != nil {
				rows.Close()
				webDBError(w, e)
				return
			}
			after = f.TargetID
			count++
			if geo.InDomain(f.URL, report.Domain) {
				items = append(items, f)
				if len(items) == 51 {
					break
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			webDBError(w, err)
			return
		}
		if count == 0 {
			break
		}
	}
	var next *string
	if len(items) > 50 {
		items = items[:50]
		cursor := strconv.FormatInt(items[49].TargetID, 10)
		next = &cursor
	}
	writeWebJSON(w, 200, map[string]any{"targets": items, "next_after": next, "total": report.PageCount})
}
