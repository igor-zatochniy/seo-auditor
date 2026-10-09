package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxSitePages = 1000
const sitemapFrontierBase = 1000

type siteCrawlOptions struct {
	RootURL     string `json:"root_url"`
	MaxPages    int    `json:"max_pages"`
	MaxDepth    int    `json:"max_depth"`
	UseSitemaps bool   `json:"use_sitemaps"`
}

type siteCrawl struct {
	Origin       string
	MaxPages     int
	MaxDepth     int
	UseSitemaps  bool
	SitemapState string
}

func normalizeSiteURL(raw string, allowPrivate bool) (string, string, error) {
	if len(raw) > 2048 || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", "", fmt.Errorf("некоректна довжина або кодування URL")
	}
	normalized, err := normalizeTargetURL(raw, allowPrivate)
	if err != nil {
		return "", "", fmt.Errorf("потрібен дозволений HTTP(S) URL без credentials")
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return "", "", err
	}
	authority, err := crawler.NormalizeAuthority(u)
	if err != nil {
		return "", "", fmt.Errorf("некоректний hostname")
	}
	u.Host = authority
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	if len(u.String()) > 2048 {
		return "", "", fmt.Errorf("URL перевищує 2048 байтів")
	}
	return u.String(), u.Scheme + "://" + authority, nil
}

func validateSiteOptions(options *siteCrawlOptions, allowPrivate bool) error {
	if options.MaxPages < 1 || options.MaxPages > maxSitePages || options.MaxDepth < 0 || options.MaxDepth > 10 {
		return fmt.Errorf("site crawl: 1–1000 сторінок, глибина 0–10")
	}
	root, _, err := normalizeSiteURL(options.RootURL, allowPrivate)
	if err != nil {
		return err
	}
	options.RootURL = root
	return nil
}

func rollbackSiteTransaction(tx pgx.Tx, cfg Config) {
	ctx, cancel := context.WithTimeout(context.Background(), effectiveDBWriteTimeout(cfg))
	defer cancel()
	_ = tx.Rollback(ctx)
}

func createSiteAuditRun(ctx context.Context, pool *pgxpool.Pool, cfg *Config, options siteCrawlOptions) error {
	if err := validateSiteOptions(&options, cfg.AllowPrivateTargets); err != nil {
		return err
	}
	_, origin, _ := normalizeSiteURL(options.RootURL, cfg.AllowPrivateTargets)
	ctx, cancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackSiteTransaction(tx, *cfg)
	if _, err = tx.Exec(ctx, `INSERT INTO audit_runs(id,started_at,heartbeat_at,worker_instance_id,owner_generation,status,targets_captured_at,total_urls,render_javascript)
		VALUES($1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,$2,1,'running',CURRENT_TIMESTAMP,1,$3)`, cfg.RunID, effectiveWorkerInstanceID(*cfg), cfg.RenderJavaScript); err != nil {
		return err
	}
	state := "pending"
	if !options.UseSitemaps {
		state = "disabled"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_site_crawls(run_id,origin,root_safe_url,key_check,max_pages,max_depth,use_sitemaps,sitemap_state)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, cfg.RunID, origin, redactURL(options.RootURL), fingerprintURL(cfg.TargetFingerprintKey, "site-crawl:"+cfg.RunID), options.MaxPages, options.MaxDepth, options.UseSitemaps, state); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_run_targets(run_id,target_id,request_url) VALUES($1,1,$2)`, cfg.RunID, options.RootURL); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_site_nodes(run_id,target_id,url_fingerprint,safe_url,frontier_depth) VALUES($1,1,$2,$3,0)`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, options.RootURL), redactURL(options.RootURL)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	cfg.OwnerGeneration = 1
	return nil
}

func loadSiteCrawl(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*siteCrawl, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.DBFetchTimeout)
	defer cancel()
	var site siteCrawl
	var keyCheck []byte
	err := pool.QueryRow(ctx, `SELECT origin,max_pages,max_depth,use_sitemaps,sitemap_state,key_check FROM audit_site_crawls WHERE run_id=$1`, cfg.RunID).
		Scan(&site.Origin, &site.MaxPages, &site.MaxDepth, &site.UseSitemaps, &site.SitemapState, &keyCheck)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(keyCheck, fingerprintURL(cfg.TargetFingerprintKey, "site-crawl:"+cfg.RunID)) {
		return nil, fmt.Errorf("site crawl requires the original fingerprint key to resume")
	}
	return &site, nil
}

// A frontier advances only after its results and newly discovered targets commit.
func claimSiteTargetBatch(ctx context.Context, pool *pgxpool.Pool, cfg Config, limit int) ([]targetURLRecord, error) {
	for {
		var depth *int
		err := withDBReadRetry(ctx, cfg, "site_frontier", func(qctx context.Context) error {
			return pool.QueryRow(qctx, `SELECT (SELECT MIN(n.frontier_depth) FROM audit_site_nodes n JOIN audit_run_targets t USING(run_id,target_id)
				WHERE n.run_id=r.id AND t.status IN ('pending','running')) FROM audit_runs r
				WHERE r.id=$1 AND r.status='running' AND r.worker_instance_id=$2 AND r.owner_generation=$3`, cfg.RunID, effectiveWorkerInstanceID(cfg), effectiveOwnerGeneration(cfg)).Scan(&depth)
		})
		if err != nil {
			return nil, err
		}
		if depth == nil {
			return nil, nil
		}
		batch, err := claimTargetURLBatch(ctx, pool, cfg, limit, *depth)
		if err != nil || len(batch) > 0 {
			for i := range batch {
				batch[i].DiscoverLinks = true
			}
			return batch, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
