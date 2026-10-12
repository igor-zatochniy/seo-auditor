package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxSitemapFiles = 16
const maxSitemapBytes = 2 << 20

func robotsSitemapLocations(body string) []string {
	var locations []string
	for _, line := range strings.FieldsFunc(strings.TrimPrefix(body, "\ufeff"), func(r rune) bool { return r == '\r' || r == '\n' }) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "sitemap") {
			continue
		}
		value, _, _ = strings.Cut(value, "#")
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 2048 {
			locations = append(locations, strings.Clone(value))
		}
		if len(locations) == maxSitemapFiles {
			break
		}
	}
	return locations
}

func (c *robotsPolicyCache) sitemapLocations(ctx context.Context, client *http.Client, origin string, timeout time.Duration) ([]string, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	key, err := robotsPolicyCacheKey(u)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := c.policy(ctx, key, func() (robotsPolicy, error) { return fetchRobotsPolicy(ctx, client, u) })
	return p.sitemaps, err
}

// XML обмежено декодованими байтами, вкладеністю, токенами та кількістю URL.
func parseSitemap(ctx context.Context, reader io.Reader, limit int, annotations ...map[string]*international.Evidence) (urls []string, index, limited bool, err error) {
	bounded := &io.LimitedReader{R: reader, N: maxSitemapBytes + 1}
	d := xml.NewDecoder(bounded)
	stack := make([]string, 0, 8)
	var loc strings.Builder
	inLoc := false
	root := ""
	var pageURL string
	var evidence *international.Evidence
	for tokens := 0; tokens < 100000; tokens++ {
		if err = ctx.Err(); err != nil {
			return
		}
		var token xml.Token
		token, err = d.Token()
		if bounded.N <= 0 {
			err = fmt.Errorf("sitemap exceeds decoded byte budget")
			return
		}
		if errors.Is(err, io.EOF) {
			if root == "" {
				err = fmt.Errorf("empty sitemap")
			} else {
				err = nil
			}
			return
		}
		if err != nil {
			return
		}
		switch t := token.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if len(stack) > 32 {
				err = fmt.Errorf("sitemap nesting exceeds budget")
				return
			}
			if len(stack) == 1 {
				if root != "" {
					err = fmt.Errorf("multiple sitemap roots")
					return
				}
				root = t.Name.Local
				index = root == "sitemapindex"
				if root != "urlset" && !index {
					err = fmt.Errorf("unsupported sitemap root")
					return
				}
			}
			if len(stack) == 3 && t.Name.Local == "loc" && ((!index && stack[1] == "url") || (index && stack[1] == "sitemap")) {
				inLoc = true
				loc.Reset()
			}
			if !index && len(stack) == 2 && t.Name.Local == "url" {
				pageURL = ""
				evidence = &international.Evidence{Links: []international.Alternate{}, Complete: true}
			}
			if !index && len(annotations) > 0 && len(stack) == 3 && stack[1] == "url" && t.Name.Local == "link" && t.Name.Space == "http://www.w3.org/1999/xhtml" && evidence != nil {
				var rel, lang, href string
				for _, a := range t.Attr {
					if a.Name.Space != "" {
						continue
					}
					switch a.Name.Local {
					case "rel":
						rel = a.Value
					case "hreflang":
						lang = a.Value
					case "href":
						href = a.Value
					}
				}
				if strings.EqualFold(strings.TrimSpace(rel), "alternate") && lang != "" {
					if len(evidence.Links) >= international.MaxAlternates || len(href) > 2048 || len(lang) > 64 {
						evidence.Complete = false
					} else {
						u, e := url.Parse(href)
						valid := e == nil && u.IsAbs() && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.Fragment == ""
						evidence.Links = append(evidence.Links, international.Alternate{Language: strings.TrimSpace(lang), URL: href, Absolute: valid, ValidURL: valid})
					}
				}
			}
		case xml.CharData:
			if inLoc {
				if loc.Len()+len(t) > 2048 {
					err = fmt.Errorf("sitemap URL exceeds budget")
					return
				}
				loc.Write(t)
			}
		case xml.EndElement:
			if len(stack) == 3 && inLoc {
				value := strings.TrimSpace(loc.String())
				inLoc = false
				if value != "" {
					if len(urls) >= limit {
						limited = true
						return
					}
					urls = append(urls, value)
					pageURL = value
				}
			}
			if !index && len(stack) == 2 && t.Name.Local == "url" && pageURL != "" && evidence != nil && len(annotations) > 0 && len(annotations[0]) < limit {
				if old := annotations[0][pageURL]; old != nil {
					old.Links = append(old.Links, evidence.Links...)
					old.Complete = old.Complete && evidence.Complete
					if len(old.Links) > international.MaxAlternates {
						old.Links = old.Links[:international.MaxAlternates]
						old.Complete = false
					}
				} else {
					annotations[0][pageURL] = evidence
				}
			}
			stack = stack[:len(stack)-1]
		}
	}
	err = fmt.Errorf("sitemap token budget exceeded")
	return
}

func fetchSitemap(ctx context.Context, client, robotsClient *http.Client, cache *robotsPolicyCache, cfg Config, site *siteCrawl, raw string, limit int, annotations ...map[string]*international.Evidence) ([]string, bool, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.HTTPTotalTimeout)
	defer cancel()
	for redirects := 0; redirects <= MaxRobotsRedirects; redirects++ {
		normalized, origin, err := normalizeSiteURL(raw, cfg.AllowPrivateTargets)
		if err != nil || origin != site.Origin {
			return nil, false, false, fmt.Errorf("sitemap is outside the crawl origin")
		}
		allowed, err := cache.isAllowedByRobots(ctx, robotsClient, normalized, cfg.RobotsTotalTimeout)
		if err != nil || !allowed {
			return nil, false, false, fmt.Errorf("sitemap robots verification did not allow access")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
		if err != nil {
			return nil, false, false, err
		}
		req.Header.Set("User-Agent", UserAgentStr)
		req.Header.Set("Accept", "application/xml,text/xml,application/gzip")
		resp, err := client.Do(req)
		if err != nil {
			return nil, false, false, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location, err := resp.Location()
			resp.Body.Close()
			if err != nil {
				return nil, false, false, err
			}
			raw = location.String()
			continue
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			resp.Body.Close()
			return nil, false, false, nil
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, false, false, fmt.Errorf("sitemap returned HTTP %d", resp.StatusCode)
		}
		defer resp.Body.Close()
		buffer := bufio.NewReader(io.LimitReader(resp.Body, maxSitemapBytes+1))
		var reader io.Reader = buffer
		magic, _ := buffer.Peek(2)
		if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			z, err := gzip.NewReader(buffer)
			if err != nil {
				return nil, false, false, err
			}
			defer z.Close()
			reader = z
		}
		return parseSitemap(ctx, reader, limit, annotations...)
	}
	return nil, false, false, fmt.Errorf("sitemap redirect budget exceeded")
}

func initializeSiteSitemaps(ctx context.Context, pool *pgxpool.Pool, cfg Config, site *siteCrawl, client, robotsClient *http.Client, cache *robotsPolicyCache) error {
	if !site.UseSitemaps || site.SitemapState != "pending" {
		return nil
	}
	if site.CompetitorSelection != nil {
		return initializeCompetitorSitemaps(ctx, pool, cfg, site, client, robotsClient, cache)
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	locations, robotsErr := cache.sitemapLocations(ctx, robotsClient, site.Origin, cfg.RobotsTotalTimeout)
	queue := append([]string{}, locations...)
	queue = append(queue, site.Origin+"/sitemap.xml")
	seen := make(map[string]bool)
	seeds := make(map[string]bool)
	state, warning := "done", ""
	if robotsErr != nil {
		state = "partial"
		warning = "Не вдалося перевірити robots.txt для sitemap"
		queue = nil
	}
	limited := false
	for len(queue) > 0 && len(seen) < maxSitemapFiles && len(seeds) < site.MaxPages {
		raw := queue[0]
		queue = queue[1:]
		normalized, origin, err := normalizeSiteURL(raw, cfg.AllowPrivateTargets)
		if err != nil || origin != site.Origin {
			state = "partial"
			warning = "Sitemap поза початковим origin пропущено"
			continue
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		annotations := map[string]*international.Evidence{}
		urls, index, cutoff, err := fetchSitemap(ctx, client, robotsClient, cache, cfg, site, normalized, site.MaxPages, annotations)
		if ctx.Err() != nil {
			if parentCtx.Err() != nil {
				return parentCtx.Err()
			}
			state = "partial"
			warning = "Вичерпано часовий бюджет sitemap"
			limited = true
			break
		}
		if err != nil {
			state = "partial"
			warning = "Один із sitemap недоступний або перевищує ліміти"
			continue
		}
		limited = limited || cutoff
		if index {
			for _, u := range urls {
				if len(queue) < maxSitemapFiles {
					queue = append(queue, u)
				} else {
					limited = true
				}
			}
			continue
		}
		if err := saveSitemapHreflang(ctx, pool, cfg, site, annotations); err != nil {
			return err
		}
		for _, u := range urls {
			normalized, origin, err := normalizeSiteURL(u, cfg.AllowPrivateTargets)
			if err != nil || origin != site.Origin {
				continue
			}
			if len(seeds) >= site.MaxPages {
				limited = true
				break
			}
			seeds[normalized] = true
		}
	}
	if len(queue) > 0 {
		limited = true
	}
	if limited {
		state = "partial"
		warning = "Досягнуто ліміт sitemap або кількості сторінок"
	}
	ctx = parentCtx
	// Order is deterministic, including after an interrupted discovery phase.
	candidates := sortedSitemapCandidates(seeds)
	for len(candidates) > 0 {
		batch := candidates[:min(len(candidates), 100)]
		candidates = candidates[len(batch):]
		if err := withDBMutationRetry(ctx, cfg, "capture_sitemap_targets", func(qctx context.Context) error {
			tx, err := pool.Begin(qctx)
			if err != nil {
				return err
			}
			defer rollbackSiteTransaction(tx, cfg)
			if err = lockSiteOwner(qctx, tx, cfg); err != nil {
				return err
			}
			if err = addSiteTargets(qctx, tx, cfg, site, batch); err != nil {
				return err
			}
			return tx.Commit(qctx)
		}); err != nil {
			return err
		}
	}
	return withDBMutationRetry(ctx, cfg, "complete_sitemap_discovery", func(qctx context.Context) error {
		tag, err := pool.Exec(qctx, `UPDATE audit_site_crawls s SET sitemap_state=$2,discovery_warning=$3,limit_reached=limit_reached OR $4
			WHERE run_id=$1 AND EXISTS(SELECT 1 FROM audit_runs r WHERE r.id=s.run_id AND r.status='running' AND r.worker_instance_id=$5 AND r.owner_generation=$6)`, cfg.RunID, state, warning, limited, effectiveWorkerInstanceID(cfg), effectiveOwnerGeneration(cfg))
		if err == nil && tag.RowsAffected() != 1 {
			return fmt.Errorf("site ownership lost during sitemap discovery")
		}
		return err
	})
}

func sortedSitemapCandidates(seeds map[string]bool) []siteCandidate {
	urls := make([]string, 0, len(seeds))
	for u := range seeds {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	candidates := make([]siteCandidate, 0, len(urls))
	for _, u := range urls {
		candidates = append(candidates, siteCandidate{url: u, depth: sitemapFrontierBase, sitemap: true})
	}
	return candidates
}
