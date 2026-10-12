package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const geoDiscoveryVersion = geo.DiscoveryVersion

type competitorDiscovery struct {
	Version   string                      `json:"version"`
	QueryHash string                      `json:"query_hash"`
	Queries   []string                    `json:"queries"`
	Summary   *competitorDiscoverySummary `json:"summary,omitempty"`
}

type competitorDiscoverySummary struct {
	Version          string `json:"version"`
	State            string `json:"state"`
	CandidateURLs    int    `json:"candidate_urls"`
	SelectedURLs     int    `json:"selected_urls"`
	MatchingQueries  int    `json:"matching_queries"`
	QueryCount       int    `json:"query_count"`
	SitemapFiles     int    `json:"sitemap_files"`
	DiscoveryLimited bool   `json:"discovery_limited"`
	RankingLimited   bool   `json:"ranking_limited"`
	SelectionLimited bool   `json:"selection_limited"`
	Warning          string `json:"warning"`
}

func prepareCompetitorDiscovery(ctx context.Context, tx pgx.Tx, cfg Config, reportID string) (*competitorDiscovery, error) {
	rows, err := tx.Query(ctx, "SELECT result->>'query' FROM geo_query_results WHERE report_id=$1 ORDER BY ordinal LIMIT 1001", reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	queries := []string{}
	for rows.Next() {
		var query string
		if err = rows.Scan(&query); err != nil {
			return nil, err
		}
		queries = append(queries, query)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(queries) == 0 || len(queries) > geo.MaxQueries {
		return nil, fmt.Errorf("competitor discovery requires 1–1000 queries")
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i] < queries[j] })
	raw, err := json.Marshal(queries)
	if err != nil {
		return nil, err
	}
	return &competitorDiscovery{Version: geoDiscoveryVersion, QueryHash: hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, geoDiscoveryVersion+":"+string(raw))), Queries: queries}, nil
}

func initializeCompetitorSitemaps(parent context.Context, pool *pgxpool.Pool, cfg Config, site *siteCrawl, client, robotsClient *http.Client, cache *robotsPolicyCache) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	summary := &competitorDiscoverySummary{Version: geoDiscoveryVersion, State: "done", QueryCount: len(site.CompetitorSelection.Queries)}
	locations, err := cache.sitemapLocations(ctx, robotsClient, site.Origin, cfg.RobotsTotalTimeout)
	queue := append(append([]string{}, locations...), site.Origin+"/sitemap.xml")
	if err != nil {
		summary.State, summary.Warning = "partial", "Не вдалося перевірити robots.txt; sitemap не завантажувались."
		queue = nil
	}
	seen, seeds := map[string]bool{}, map[string]bool{}
	for len(queue) > 0 && len(seen) < maxSitemapFiles && len(seeds) < geo.MaxDiscoveryURLs {
		raw := queue[0]
		queue = queue[1:]
		normalized, origin, err := normalizeSiteURL(raw, cfg.AllowPrivateTargets)
		if err != nil || origin != site.Origin {
			summary.State, summary.Warning = "partial", "Sitemap поза початковим origin пропущено."
			continue
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		summary.SitemapFiles++
		urls, index, cutoff, err := fetchSitemap(ctx, client, robotsClient, cache, cfg, site, normalized, geo.MaxDiscoveryURLs)
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			summary.State, summary.Warning = "partial", "Вичерпано часовий бюджет sitemap."
			summary.DiscoveryLimited = true
			break
		}
		if err != nil {
			summary.State, summary.Warning = "partial", "Один із sitemap недоступний або перевищує безпечні ліміти."
			continue
		}
		summary.DiscoveryLimited = summary.DiscoveryLimited || cutoff
		if index {
			for _, candidate := range urls {
				if len(queue) < maxSitemapFiles {
					queue = append(queue, candidate)
				} else {
					summary.DiscoveryLimited = true
				}
			}
			continue
		}
		for _, candidate := range urls {
			normalized, origin, err := normalizeSiteURL(candidate, cfg.AllowPrivateTargets)
			if err != nil || origin != site.Origin {
				continue
			}
			u, err := url.Parse(normalized)
			if err != nil || u.RawQuery != "" || u.ForceQuery || u.Path == "/" {
				continue
			}
			if len(seeds) == geo.MaxDiscoveryURLs && !seeds[normalized] {
				summary.DiscoveryLimited = true
				break
			}
			seeds[normalized] = true
		}
	}
	if len(queue) > 0 {
		summary.DiscoveryLimited = true
	}
	urls := make([]string, 0, len(seeds))
	for candidate := range seeds {
		urls = append(urls, candidate)
	}
	rankCtx, stopRanking := context.WithTimeout(parent, 10*time.Second)
	selection, err := geo.SelectDiscoveryURLs(rankCtx, site.CompetitorSelection.Queries, urls, max(0, site.MaxPages-1))
	stopRanking()
	if parent.Err() != nil {
		return parent.Err()
	}
	if err != nil {
		summary.State, summary.Warning = "partial", "Не вдалося завершити відбір URL; залишається обмежений обхід посилань."
		summary.RankingLimited = true
		selection.URLs = nil
	}
	summary.CandidateURLs, summary.SelectedURLs = len(seeds), 1+len(selection.URLs)
	summary.MatchingQueries, summary.RankingLimited = selection.MatchedQueries, summary.RankingLimited || selection.Limited
	summary.SelectionLimited = len(seeds) > len(selection.URLs)
	if summary.DiscoveryLimited || summary.RankingLimited {
		summary.State = "partial"
		if summary.Warning == "" {
			summary.Warning = "Discovery або ранжування обмежене; неперевірені URL не вважаються відсутніми."
		}
	}
	if len(seeds) == 0 && summary.Warning == "" {
		summary.Warning = "URL у sitemap не знайдено; використовується обмежений обхід посилань."
	}
	manifest := *site.CompetitorSelection
	manifest.Summary = summary
	candidates := make([]siteCandidate, 0, len(selection.URLs))
	for _, raw := range selection.URLs {
		candidates = append(candidates, siteCandidate{url: raw, depth: sitemapFrontierBase, sitemap: true})
	}
	// Відбір і завершення discovery атомарні: restart не залишає наполовину виданий набір.
	return withDBMutationRetry(parent, cfg, "select_competitor_targets", func(qctx context.Context) error {
		tx, err := pool.Begin(qctx)
		if err != nil {
			return err
		}
		defer rollbackSiteTransaction(tx, cfg)
		if err = lockSiteOwner(qctx, tx, cfg); err != nil {
			return err
		}
		if err = addSiteTargets(qctx, tx, cfg, site, candidates); err != nil {
			return err
		}
		if _, err = tx.Exec(qctx, `UPDATE audit_site_crawls SET competitor_selection=$2,sitemap_state=$3,discovery_warning=$4,
			limit_reached=limit_reached OR $5 WHERE run_id=$1`, cfg.RunID, manifest, summary.State, summary.Warning,
			summary.DiscoveryLimited || summary.RankingLimited || summary.SelectionLimited); err != nil {
			return err
		}
		return tx.Commit(qctx)
	})
}
