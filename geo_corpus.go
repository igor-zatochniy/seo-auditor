package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

const geoCorpusBatchSize = 32
const geoOperationTimeout = 5 * time.Minute
const geoIndexModel = geo.Model + "/postgres-v1"

func isGEOCorpusOperation(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/api/geo/reports" || strings.HasSuffix(r.URL.Path, "/target") || strings.HasSuffix(r.URL.Path, "/visibility/imports")) ||
		r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/geo/reports/") &&
			(strings.HasSuffix(r.URL.Path, "/opportunities") || strings.HasSuffix(r.URL.Path, "/tasks.csv") || strings.HasSuffix(r.URL.Path, "/tasks.md") || strings.HasSuffix(r.URL.Path, "/tasks.json"))
}

type geoCorpusSummary struct {
	Backend       string `json:"backend"`
	SourcePages   int    `json:"source_pages"`
	Pages         int    `json:"pages"`
	Excluded      int    `json:"excluded_other_domains"`
	LimitedPages  int    `json:"limited_pages"`
	Complete      bool   `json:"complete"`
	CrawlComplete *bool  `json:"crawl_complete"`
	CrawlWarning  string `json:"crawl_warning,omitempty"`
}

type postgresGEOCorpus struct {
	tx             pgx.Tx
	runID          string
	domain         string
	summary        geoCorpusSummary
	wordsCache     map[string][]string
	wordEntries    int
	frequencyCache map[[32]byte]int
	candidateCache map[int64]geoCachedCandidate
}

type geoCachedCandidate struct {
	page    geo.Page
	profile geo.LexicalProfile
}

// Копіюється лише bounded лексичний профіль, а не повний HTML або request_url.
const geoPageProjection = `target_id,safe_url,LEFT(COALESCE(title,''),8192),LEFT(COALESCE(description,''),8192),LEFT(COALESCE(h1,''),8192),
 COALESCE(h1_count,0),COALESCE(external_links_count,0),COALESCE(word_count,0),LEFT(COALESCE(meta_robots,''),8192),LEFT(COALESCE(x_robots_tag,''),8192),
 LEFT(COALESCE(canonical_url,''),2048),COALESCE(is_self_canonical,FALSE),geo_signals,
 char_length(COALESCE(title,''))>8192 OR char_length(COALESCE(description,''))>8192 OR char_length(COALESCE(h1,''))>8192`

func scanGEOPage(row pgx.Row) (geo.Page, bool, error) {
	var page geo.Page
	var limited bool
	err := row.Scan(&page.TargetID, &page.URL, &page.Title, &page.Description, &page.H1, &page.H1Count, &page.ExternalLinks, &page.WordCount,
		&page.MetaRobots, &page.XRobotsTag, &page.Canonical, &page.SelfCanonical, &page.Signals, &limited)
	return page, limited, err
}

func ensureGEOCorpus(ctx context.Context, tx pgx.Tx, runID, domain string) (*postgresGEOCorpus, error) {
	c := &postgresGEOCorpus{tx: tx, runID: runID, domain: domain, summary: geoCorpusSummary{Backend: "postgres-v1"}}
	var crawlComplete bool
	crawlErr := tx.QueryRow(ctx, `SELECT graph_ready AND NOT limit_reached AND sitemap_state NOT IN ('pending','partial')
 AND NOT EXISTS(SELECT 1 FROM audit_site_nodes WHERE run_id=$1 AND links_truncated) FROM audit_site_crawls WHERE run_id=$1`, runID).Scan(&crawlComplete)
	if crawlErr != nil && !errors.Is(crawlErr, pgx.ErrNoRows) {
		return nil, crawlErr
	}
	if crawlErr == nil {
		c.summary.CrawlComplete = &crawlComplete
	}
	if c.summary.CrawlComplete == nil {
		c.summary.CrawlWarning = "Аудит списку URL: повноту покриття сайту не підтверджено."
	} else if !crawlComplete {
		c.summary.CrawlWarning = "SEO-обхід обмежений або неповний. Індекс містить усі успішні сторінки цього завершеного запуску, не весь сайт."
	}
	err := tx.QueryRow(ctx, `SELECT source_pages,page_count,limited_pages,ready FROM geo_corpora WHERE run_id=$1 AND domain=$2 AND model=$3`, runID, domain, geoIndexModel).
		Scan(&c.summary.SourcePages, &c.summary.Pages, &c.summary.LimitedPages, &c.summary.Complete)
	if err == nil {
		if !c.summary.Complete {
			return nil, errors.New("GEO corpus is not complete")
		}
		c.summary.Excluded = c.summary.SourcePages - c.summary.Pages
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var locked bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(8247310004)").Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, errGEOAnalysisBusy
	}
	var expected int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_results WHERE run_id=$1 AND scan_status='completed' AND status_code=200`, runID).Scan(&expected); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO geo_corpora(run_id,domain,model) VALUES($1,$2,$3)`, runID, domain, geoIndexModel); err != nil {
		return nil, err
	}
	after := int64(math.MinInt64)
	for {
		rows, err := tx.Query(ctx, `SELECT `+geoPageProjection+` FROM audit_results
 WHERE run_id=$1 AND target_id>$2 AND scan_status='completed' AND status_code=200 ORDER BY target_id LIMIT $3`, runID, after, geoCorpusBatchSize)
		if err != nil {
			return nil, err
		}
		pages, profiles := []geo.Page{}, []geo.LexicalProfile{}
		read := 0
		for rows.Next() {
			page, limited, scanErr := scanGEOPage(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			after = page.TargetID
			read++
			c.summary.SourcePages++
			if !geo.InDomain(page.URL, domain) {
				continue
			}
			profile, profileErr := geo.BuildLexicalProfile(ctx, page)
			if profileErr != nil {
				rows.Close()
				return nil, profileErr
			}
			profile.Limited = profile.Limited || limited
			if profile.Limited {
				c.summary.LimitedPages++
			}
			pages, profiles = append(pages, page), append(profiles, profile)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if read == 0 {
			break
		}
		if err = c.insertBatch(ctx, pages, profiles); err != nil {
			return nil, err
		}
		c.summary.Pages += len(pages)
	}
	if expected != c.summary.SourcePages {
		return nil, fmt.Errorf("incomplete GEO source: expected %d, checked %d", expected, c.summary.SourcePages)
	}
	if c.summary.Pages == 0 {
		return nil, geoInputError{"У цьому аудиті немає успішних HTML-сторінок указаного домену."}
	}
	if _, err = tx.Exec(ctx, `UPDATE geo_corpora SET source_pages=$4,page_count=$5,limited_pages=$6,ready=TRUE WHERE run_id=$1 AND domain=$2 AND model=$3`,
		runID, domain, geoIndexModel, expected, c.summary.Pages, c.summary.LimitedPages); err != nil {
		return nil, err
	}
	c.summary.Complete, c.summary.Excluded = true, expected-c.summary.Pages
	return c, nil
}

func (c *postgresGEOCorpus) insertBatch(ctx context.Context, pages []geo.Page, profiles []geo.LexicalProfile) error {
	if len(pages) == 0 {
		return nil
	}
	docs, postings := make([][]any, 0, len(pages)), [][]any{}
	dictionary := map[string]geo.LexicalTerm{}
	for i, p := range pages {
		profile := profiles[i]
		docs = append(docs, []any{c.runID, c.domain, geoIndexModel, p.TargetID, profile.Phrases, profile.Limited})
		for _, term := range profile.Terms {
			postings = append(postings, []any{c.runID, c.domain, geoIndexModel, p.TargetID, term.Term, term.Weight})
			dictionary[term.Term] = term
		}
	}
	if _, err := c.tx.CopyFrom(ctx, pgx.Identifier{"geo_corpus_pages"}, []string{"run_id", "domain", "model", "target_id", "phrases", "limited"}, pgx.CopyFromRows(docs)); err != nil {
		return err
	}
	if _, err := c.tx.CopyFrom(ctx, pgx.Identifier{"geo_corpus_terms"}, []string{"run_id", "domain", "model", "target_id", "term", "weight"}, pgx.CopyFromRows(postings)); err != nil {
		return err
	}
	terms := make([]geo.LexicalTerm, 0, len(dictionary))
	for _, term := range dictionary {
		terms = append(terms, term)
	}
	data, err := json.Marshal(terms)
	if err != nil {
		return err
	}
	_, err = c.tx.Exec(ctx, `INSERT INTO geo_corpus_dictionary(run_id,domain,model,term,family,first_rune,length,fuzzy)
 SELECT $1,$2,$3,"Term","Family","First","Length","Fuzzy" FROM jsonb_to_recordset($4) AS t("Term" text,"Family" text,"First" text,"Length" smallint,"Fuzzy" boolean)
 ON CONFLICT DO NOTHING`, c.runID, c.domain, geoIndexModel, data)
	return err
}

func (c *postgresGEOCorpus) Forms(ctx context.Context, family string) ([]string, error) {
	return c.cachedWords(ctx, "family:"+family, `SELECT term FROM geo_corpus_dictionary WHERE run_id=$1 AND domain=$2 AND model=$3 AND family=$4 ORDER BY term COLLATE "C"`, family)
}

func (c *postgresGEOCorpus) FuzzyBucket(ctx context.Context, first string, length int) ([]string, error) {
	return c.cachedWords(ctx, fmt.Sprintf("fuzzy:%s:%d", first, length), `SELECT term FROM geo_corpus_dictionary WHERE run_id=$1 AND domain=$2 AND model=$3 AND fuzzy AND first_rune=$4 AND length=$5 ORDER BY term COLLATE "C" LIMIT 257`, first, length)
}

func (c *postgresGEOCorpus) cachedWords(ctx context.Context, key, sql string, args ...any) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if words, ok := c.wordsCache[key]; ok {
		return words, nil
	}
	words, err := c.words(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(c.wordsCache) < 4096 && len(words) <= 257 && c.wordEntries+len(words) <= 8192 {
		if c.wordsCache == nil {
			c.wordsCache = map[string][]string{}
		}
		c.wordsCache[key] = words
		c.wordEntries += len(words)
	}
	return words, nil
}

func (c *postgresGEOCorpus) words(ctx context.Context, sql string, args ...any) ([]string, error) {
	args = append([]any{c.runID, c.domain, geoIndexModel}, args...)
	rows, err := c.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	words := []string{}
	for rows.Next() {
		var word string
		if err = rows.Scan(&word); err != nil {
			return nil, err
		}
		words = append(words, word)
	}
	return words, rows.Err()
}

func (c *postgresGEOCorpus) Frequency(ctx context.Context, words []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	keys := append([]string(nil), words...)
	sort.Strings(keys)
	key := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	if count, ok := c.frequencyCache[key]; ok {
		return count, nil
	}
	var count int
	err := c.tx.QueryRow(ctx, `SELECT COUNT(DISTINCT target_id) FROM geo_corpus_terms WHERE run_id=$1 AND domain=$2 AND model=$3 AND term=ANY($4::text[])`,
		c.runID, c.domain, geoIndexModel, words).Scan(&count)
	if err == nil && len(c.frequencyCache) < 4096 {
		if c.frequencyCache == nil {
			c.frequencyCache = map[[32]byte]int{}
		}
		c.frequencyCache[key] = count
	}
	return count, err
}

// SQL оцінює всі matching targets; LIMIT застосовується лише після повного ranking.
// Незалежні внески не створюють повторного scan профілів на кожен target
// за відсутності статистики щойно записаного корпусу.
// OFFSET 0 зберігає параметризований index lookup для кожного варіанта: оцінка
// jsonb_to_recordset не повинна спричиняти повний scan postings на кожен запит.
const geoCandidateScoresSQL = `WITH variants AS (
 SELECT * FROM jsonb_to_recordset($4) AS v(index int,term text,kind text,quality double precision,idf double precision)
), best AS (
 SELECT DISTINCT ON (t.target_id,v.index) t.target_id,v.index,v.idf,v.quality,t.weight
 FROM variants v CROSS JOIN LATERAL (
 SELECT target_id,term,weight FROM geo_corpus_terms
 WHERE run_id=$1 AND domain=$2 AND model=$3 AND term=v.term OFFSET 0
 ) t
 ORDER BY t.target_id,v.index,v.quality DESC,t.weight DESC,t.term COLLATE "C"
), scores AS (
 SELECT target_id,COUNT(*) AS matched,SUM(idf*quality)/$5::double precision AS coverage,
 SUM(idf*quality*weight/4.0)/$5::double precision AS strength FROM best GROUP BY target_id
), ranked AS (
 SELECT target_id,FLOOR(100*MAX(coverage)+0.5)::int AS weighted,FLOOR(100*SUM(component)+0.5)::int AS relevance
 FROM (
 SELECT target_id,coverage,TRUE AS lexical,
 CASE WHEN cardinality($6::text[])=0 THEN 0.75*coverage+0.25*strength ELSE 0.65*coverage+0.25*strength END AS component FROM scores
 UNION ALL
 SELECT target_id,0::double precision,FALSE,
 CASE WHEN cardinality($6::text[])>0 THEN 0.10*(SELECT COUNT(*) FROM unnest($6::text[]) AS k WHERE k=ANY(p.phrases))/cardinality($6::text[]) ELSE 0 END
 FROM geo_corpus_pages p WHERE p.run_id=$1 AND p.domain=$2 AND p.model=$3 AND p.phrases && $6::text[]
 ) contributions GROUP BY target_id HAVING BOOL_OR(lexical)
) `

const geoCandidateSQL = geoCandidateScoresSQL + `SELECT target_id FROM ranked ORDER BY relevance DESC,weighted DESC,target_id LIMIT 3`

func (c *postgresGEOCorpus) rank(ctx context.Context, query string) ([]geo.RankedCandidate, error) {
	q, err := geo.PrepareIndexedQuery(ctx, query, c.summary.Pages, c)
	if err != nil {
		return nil, err
	}
	return c.rankPrepared(ctx, q)
}

func (c *postgresGEOCorpus) rankPrepared(ctx context.Context, q geo.IndexedQuery) ([]geo.RankedCandidate, error) {
	result := []geo.RankedCandidate{}
	if q.TotalIDF == 0 {
		return result, nil
	}
	data, err := json.Marshal(q.Variants)
	if err != nil {
		return nil, err
	}
	keys := q.PhraseKeys
	if keys == nil {
		keys = []string{}
	}
	rows, err := c.tx.Query(ctx, geoCandidateSQL, c.runID, c.domain, geoIndexModel, data, q.TotalIDF, keys)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		candidate, err := c.score(ctx, q, id)
		if err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Matching.Relevance != b.Matching.Relevance {
			return a.Matching.Relevance > b.Matching.Relevance
		}
		if a.Matching.WeightedCoverage != b.Matching.WeightedCoverage {
			return a.Matching.WeightedCoverage > b.Matching.WeightedCoverage
		}
		return a.Page.TargetID < b.Page.TargetID
	})
	return result, nil
}

func (c *postgresGEOCorpus) score(ctx context.Context, q geo.IndexedQuery, id int64) (geo.RankedCandidate, error) {
	if err := ctx.Err(); err != nil {
		return geo.RankedCandidate{}, err
	}
	if candidate, ok := c.candidateCache[id]; ok {
		return geo.ScoreIndexedPage(q, candidate.page, candidate.profile), nil
	}
	p, _, err := scanGEOPage(c.tx.QueryRow(ctx, `SELECT `+geoPageProjection+` FROM audit_results WHERE run_id=$1 AND target_id=$2`, c.runID, id))
	if err != nil {
		return geo.RankedCandidate{}, err
	}
	var profile geo.LexicalProfile
	if err = c.tx.QueryRow(ctx, `SELECT phrases,limited FROM geo_corpus_pages WHERE run_id=$1 AND domain=$2 AND model=$3 AND target_id=$4`,
		c.runID, c.domain, geoIndexModel, id).Scan(&profile.Phrases, &profile.Limited); err != nil {
		return geo.RankedCandidate{}, err
	}
	rows, err := c.tx.Query(ctx, `SELECT term,weight FROM geo_corpus_terms WHERE run_id=$1 AND domain=$2 AND model=$3 AND target_id=$4`, c.runID, c.domain, geoIndexModel, id)
	if err != nil {
		return geo.RankedCandidate{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var term geo.LexicalTerm
		if err = rows.Scan(&term.Term, &term.Weight); err != nil {
			return geo.RankedCandidate{}, err
		}
		profile.Terms = append(profile.Terms, term)
	}
	if err = rows.Err(); err != nil {
		return geo.RankedCandidate{}, err
	}
	if len(c.candidateCache) < geoCorpusBatchSize {
		if c.candidateCache == nil {
			c.candidateCache = map[int64]geoCachedCandidate{}
		}
		c.candidateCache[id] = geoCachedCandidate{p, profile}
	}
	return geo.ScoreIndexedPage(q, p, profile), nil
}

func (c *postgresGEOCorpus) analyze(ctx context.Context, queries []string, brand string, semanticEnabled ...bool) ([]geo.Result, error) {
	if len(queries) == 0 || len(queries) > geo.MaxQueries {
		return nil, geoInputError{"Потрібно від 1 до 1000 запитів."}
	}
	results := make([]geo.Result, 0, len(queries))
	counts := map[int64]int{}
	for _, query := range queries {
		candidates, err := c.rank(ctx, query)
		if err != nil {
			return nil, err
		}
		r := geo.ResultFromCandidates(query, candidates)
		if len(semanticEnabled) > 0 && semanticEnabled[0] {
			prepared, units, err := geo.PrepareSemanticQuery(ctx, query, c.summary.Pages, c)
			if err != nil {
				return nil, err
			}
			semanticCandidates, err := c.rankPrepared(ctx, prepared)
			if err != nil {
				return nil, err
			}
			r = geo.CombineSemantic(query, candidates, semanticCandidates, units, prepared.Limited)
			if r.Semantic.Selected {
				candidates = semanticCandidates
			}
		}
		if r.TargetID != nil {
			counts[*r.TargetID]++
			r.Entities = geo.EvaluateEntities(brand, candidates[0].Page.Signals)
		}
		results = append(results, r)
	}
	for i := range results {
		if results[i].TargetID != nil {
			results[i].SharedQueries = counts[*results[i].TargetID]
		}
	}
	return results, nil
}
