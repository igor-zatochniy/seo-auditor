package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

const siteGraphPartialSQL = `s.limit_reached OR NOT s.graph_ready OR s.sitemap_state IN ('pending','partial') OR ar.status<>'completed'
 OR EXISTS(SELECT 1 FROM audit_site_nodes sn LEFT JOIN audit_results sr USING(run_id,target_id) WHERE sn.run_id=s.run_id AND (sn.links_truncated OR sr.target_id IS NULL OR sr.status_code IS NULL OR sr.scan_status NOT IN ('completed','redirect') OR (sr.status_code<>200 AND sr.status_code NOT BETWEEN 300 AND 399)))
 OR EXISTS(SELECT 1 FROM audit_crawl_inventory ci LEFT JOIN audit_site_nodes cn ON cn.run_id=ci.run_id AND cn.url_fingerprint=ci.fingerprint LEFT JOIN audit_results cr ON cr.run_id=cn.run_id AND cr.target_id=cn.target_id WHERE ci.run_id=s.run_id AND cr.status_code IS NULL)
 OR EXISTS(SELECT 1 FROM audit_site_edges se LEFT JOIN audit_site_nodes dn ON dn.run_id=se.run_id AND dn.url_fingerprint=se.to_fingerprint LEFT JOIN audit_results dr ON dr.run_id=dn.run_id AND dr.target_id=dn.target_id WHERE se.run_id=s.run_id AND se.is_internal AND dr.status_code IS NULL)`

type reportCoverage struct {
	Root             string  `json:"root"`
	MaxPages         int     `json:"max_pages"`
	MaxDepth         int     `json:"max_depth"`
	Known            int64   `json:"known"`
	Checked          int64   `json:"checked"`
	SitemapKnown     int64   `json:"sitemap_known"`
	SitemapChecked   int64   `json:"sitemap_checked"`
	SitemapPercent   float64 `json:"sitemap_percent"`
	ResourceHints    int64   `json:"resource_hints"`
	UnsupportedTypes int64   `json:"unsupported_type"`
	SitemapState     string  `json:"sitemap_state"`
	Partial          bool    `json:"partial"`
	GraphPartial     bool    `json:"graph_partial"`
	Warning          string  `json:"warning"`
}

func loadReportCoverage(ctx context.Context, q reportQuerier, id string) (*reportCoverage, error) {
	var c reportCoverage
	err := q.QueryRow(ctx, `WITH known AS (
 SELECT fingerprint,sources,url_shape FROM audit_crawl_inventory WHERE run_id=$1
 UNION ALL SELECT n.url_fingerprint,CASE WHEN n.in_sitemap THEN 2 ELSE 0 END,NULL::JSONB FROM audit_site_nodes n
 WHERE n.run_id=$1 AND NOT EXISTS(SELECT 1 FROM audit_crawl_inventory i WHERE i.run_id=n.run_id AND i.fingerprint=n.url_fingerprint)
 ), counts AS (SELECT COUNT(*) known,COUNT(*) FILTER(WHERE r.status_code IS NOT NULL) checked,
 COUNT(*) FILTER(WHERE (k.sources&2)>0) sitemap_known,COUNT(*) FILTER(WHERE (k.sources&2)>0 AND r.status_code IS NOT NULL) sitemap_checked,
 COUNT(*) FILTER(WHERE n.target_id IS NULL AND (k.sources&2)=0 AND k.url_shape->>'resource_hint'='true') resource_hints,
 COUNT(*) FILTER(WHERE r.error_code='unsupported_content_type') unsupported_type
 FROM known k LEFT JOIN audit_site_nodes n ON n.run_id=$1 AND n.url_fingerprint=k.fingerprint LEFT JOIN audit_results r ON r.run_id=n.run_id AND r.target_id=n.target_id)
 SELECT s.root_safe_url,s.max_pages,s.max_depth,c.known,c.checked,c.sitemap_known,c.sitemap_checked,c.resource_hints,c.unsupported_type,s.sitemap_state,
 (`+siteGraphPartialSQL+`),s.discovery_warning,
 NOT COALESCE(i.captured,FALSE) OR COALESCE(i.limited,FALSE)
 FROM audit_site_crawls s JOIN audit_runs ar ON ar.id=s.run_id CROSS JOIN counts c
 LEFT JOIN audit_crawl_inventory_state i ON i.run_id=s.run_id WHERE s.run_id=$1`, id).Scan(&c.Root, &c.MaxPages, &c.MaxDepth, &c.Known, &c.Checked, &c.SitemapKnown, &c.SitemapChecked, &c.ResourceHints, &c.UnsupportedTypes, &c.SitemapState, &c.GraphPartial, &c.Warning, &c.Partial)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Root, c.Warning = redactURL(c.Root), redactText(c.Warning)
	c.Partial = c.Partial || c.GraphPartial || c.Checked < c.Known
	if c.SitemapKnown > 0 {
		c.SitemapPercent = 100 * float64(c.SitemapChecked) / float64(c.SitemapKnown)
	}
	return &c, nil
}
