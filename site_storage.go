package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/igor-zatochniy/seo-auditor/internal/seo"
	"github.com/jackc/pgx/v5"
)

type siteCandidate struct {
	url     string
	depth   int
	sitemap bool
}

func lockSiteOwner(ctx context.Context, tx pgx.Tx, cfg Config) error {
	var id string
	return tx.QueryRow(ctx, `SELECT id::TEXT FROM audit_runs WHERE id=$1 AND status='running'
		AND worker_instance_id=$2 AND owner_generation=$3 FOR UPDATE`, cfg.RunID, effectiveWorkerInstanceID(cfg), effectiveOwnerGeneration(cfg)).Scan(&id)
}

// The caller holds the run row lock. Targets and graph identities commit together.
func addSiteTargets(ctx context.Context, tx pgx.Tx, cfg Config, site *siteCrawl, candidates []siteCandidate) error {
	type node struct {
		id      int64
		depth   int
		sitemap bool
	}
	nodes := make(map[string]node)
	rows, err := tx.Query(ctx, `SELECT target_id,url_fingerprint,frontier_depth,in_sitemap FROM audit_site_nodes WHERE run_id=$1`, cfg.RunID)
	if err != nil {
		return err
	}
	var maxID int64
	for rows.Next() {
		var n node
		var fingerprint []byte
		if err = rows.Scan(&n.id, &fingerprint, &n.depth, &n.sitemap); err != nil {
			rows.Close()
			return err
		}
		nodes[string(fingerprint)] = n
		maxID = max(maxID, n.id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var targets, identities [][]any
	var changedIDs []int64
	var depths []int32
	var sitemapFlags []bool
	limited := false
	for _, candidate := range candidates {
		normalized, origin, err := normalizeSiteURL(candidate.url, cfg.AllowPrivateTargets)
		if err != nil || origin != site.Origin {
			continue
		}
		fp := fingerprintURL(cfg.TargetFingerprintKey, normalized)
		if n, ok := nodes[string(fp)]; ok {
			depth := min(n.depth, candidate.depth)
			inSitemap := n.sitemap || candidate.sitemap
			if depth != n.depth || inSitemap != n.sitemap {
				changedIDs = append(changedIDs, n.id)
				depths = append(depths, int32(depth))
				sitemapFlags = append(sitemapFlags, inSitemap)
				n.depth, n.sitemap = depth, inSitemap
				nodes[string(fp)] = n
			}
			continue
		}
		if candidate.depth%sitemapFrontierBase > site.MaxDepth || len(nodes) >= site.MaxPages {
			limited = true
			continue
		}
		maxID++
		nodes[string(fp)] = node{id: maxID, depth: candidate.depth, sitemap: candidate.sitemap}
		targets = append(targets, []any{cfg.RunID, maxID, normalized})
		identities = append(identities, []any{cfg.RunID, maxID, fp, redactURL(normalized), candidate.depth, candidate.sitemap})
	}
	if len(targets) > 0 {
		if _, err = tx.CopyFrom(ctx, pgx.Identifier{"audit_run_targets"}, []string{"run_id", "target_id", "request_url"}, pgx.CopyFromRows(targets)); err != nil {
			return err
		}
		if _, err = tx.CopyFrom(ctx, pgx.Identifier{"audit_site_nodes"}, []string{"run_id", "target_id", "url_fingerprint", "safe_url", "frontier_depth", "in_sitemap"}, pgx.CopyFromRows(identities)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE audit_runs SET total_urls=$2 WHERE id=$1`, cfg.RunID, len(nodes)); err != nil {
			return err
		}
	}
	if len(changedIDs) > 0 {
		_, err = tx.Exec(ctx, `UPDATE audit_site_nodes n SET frontier_depth=LEAST(n.frontier_depth,v.depth),in_sitemap=n.in_sitemap OR v.in_sitemap
			FROM (SELECT id,MIN(depth) depth,BOOL_OR(in_sitemap) in_sitemap FROM unnest($2::BIGINT[],$3::INT[],$4::BOOLEAN[]) v(id,depth,in_sitemap) GROUP BY id) v
			WHERE n.run_id=$1 AND n.target_id=v.id`, cfg.RunID, changedIDs, depths, sitemapFlags)
		if err != nil {
			return err
		}
	}
	if limited {
		_, err = tx.Exec(ctx, `UPDATE audit_site_crawls SET limit_reached=TRUE WHERE run_id=$1`, cfg.RunID)
	}
	return err
}

func persistSiteDiscovery(ctx context.Context, tx pgx.Tx, cfg Config, site *siteCrawl, res Result) error {
	var depth int
	if err := tx.QueryRow(ctx, `SELECT frontier_depth FROM audit_site_nodes WHERE run_id=$1 AND target_id=$2`, cfg.RunID, res.Target.TargetID).Scan(&depth); err != nil {
		return err
	}
	links := res.Data.DiscoveredLinks
	kind := "link"
	if res.Error != nil || !res.Data.HTMLSizeComplete {
		links = nil
	}
	if res.Data.IsRedirect && res.Data.RedirectURL != "" {
		base, err := url.Parse(res.Target.RequestURL)
		ref, refErr := url.Parse(res.Data.RedirectURL)
		if err == nil && refErr == nil {
			links = []seo.DiscoveredLink{{URL: base.ResolveReference(ref).String()}}
			kind = "redirect"
		}
	}
	if len(links) > seo.MaxDiscoveredLinks {
		return fmt.Errorf("site graph link budget exceeded")
	}
	var edges [][]any
	var candidates []siteCandidate
	for i, link := range links {
		normalized, origin, err := normalizeSiteURL(link.URL, cfg.AllowPrivateTargets)
		if err != nil {
			continue
		}
		internal := origin == site.Origin
		anchor, _, _ := limitStorageString(redactText(link.Anchor), 256)
		edges = append(edges, []any{cfg.RunID, res.Target.TargetID, i + 1, fingerprintURL(cfg.TargetFingerprintKey, normalized), redactURL(normalized), anchor, internal, link.Nofollow, kind})
		if internal && !link.Nofollow {
			// Redirect hops also consume the discovery budget, bounding redirect loops.
			candidates = append(candidates, siteCandidate{url: normalized, depth: depth + 1})
		}
	}
	if err := addSiteTargets(ctx, tx, cfg, site, candidates); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_site_edges WHERE run_id=$1 AND from_target_id=$2`, cfg.RunID, res.Target.TargetID); err != nil {
		return err
	}
	if len(edges) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"audit_site_edges"}, []string{"run_id", "from_target_id", "ordinal", "to_fingerprint", "safe_url", "anchor_text", "is_internal", "nofollow", "kind"}, pgx.CopyFromRows(edges)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE audit_site_nodes SET links_truncated=$3 WHERE run_id=$1 AND target_id=$2`, cfg.RunID, res.Target.TargetID, res.Data.LinksTruncated)
	return err
}
