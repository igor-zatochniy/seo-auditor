package main

import (
	"context"
	"encoding/json"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/jackc/pgx/v5"
)

const maxInventoryURLs = 50000

type inventoryRecord struct {
	Fingerprint       []byte
	URL               string
	Sources, Deferred int32
	Important         bool
	Shape             crawler.URLShape
}

func inventoryURL(cfg Config, raw string, sources, deferred int32) inventoryRecord {
	return inventoryRecord{Fingerprint: fingerprintURL(cfg.TargetFingerprintKey, raw), URL: redactURL(raw), Sources: sources, Deferred: deferred, Shape: crawler.DescribeURL(raw)}
}

// Ліміт захищений row lock; відомі URL оновлюються навіть після його досягнення.
func storeInventory(ctx context.Context, tx pgx.Tx, runID string, records []inventoryRecord) error {
	if len(records) == 0 {
		return nil
	}
	if _, e := tx.Exec(ctx, `INSERT INTO audit_crawl_inventory_state(run_id) VALUES($1) ON CONFLICT DO NOTHING`, runID); e != nil {
		return e
	}
	var count int
	if e := tx.QueryRow(ctx, `SELECT url_count FROM audit_crawl_inventory_state WHERE run_id=$1 FOR UPDATE`, runID).Scan(&count); e != nil {
		return e
	}
	for len(records) > 0 {
		batch := records[:min(len(records), 256)]
		records = records[len(batch):]
		unique := map[string]inventoryRecord{}
		order := []string{}
		fps := [][]byte{}
		for _, v := range batch {
			key := string(v.Fingerprint)
			if old, ok := unique[key]; ok {
				v.Sources |= old.Sources
				v.Deferred |= old.Deferred
				v.Important = v.Important || old.Important
			} else {
				order = append(order, key)
				fps = append(fps, v.Fingerprint)
			}
			unique[key] = v
		}
		rows, e := tx.Query(ctx, `SELECT fingerprint FROM audit_crawl_inventory WHERE run_id=$1 AND fingerprint=ANY($2::BYTEA[])`, runID, fps)
		if e != nil {
			return e
		}
		exists := map[string]bool{}
		for rows.Next() {
			var fp []byte
			if e = rows.Scan(&fp); e != nil {
				rows.Close()
				return e
			}
			exists[string(fp)] = true
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		fps = nil
		urls := []string{}
		sources, reasons := []int32{}, []int32{}
		important := []bool{}
		shapes := []string{}
		limited := false
		for _, key := range order {
			v := unique[key]
			if !exists[key] {
				if count >= maxInventoryURLs {
					limited = true
					continue
				}
				count++
			}
			raw, e := json.Marshal(v.Shape)
			if e != nil {
				return e
			}
			fps = append(fps, v.Fingerprint)
			urls = append(urls, v.URL)
			sources = append(sources, v.Sources)
			reasons = append(reasons, v.Deferred)
			important = append(important, v.Important)
			shapes = append(shapes, string(raw))
		}
		if len(fps) > 0 {
			_, e = tx.Exec(ctx, `INSERT INTO audit_crawl_inventory(run_id,fingerprint,safe_url,sources,deferred,important,url_shape)
    SELECT $1,v.fp,v.url,v.source,v.reason,v.important,v.shape::JSONB FROM unnest($2::BYTEA[],$3::TEXT[],$4::INT[],$5::INT[],$6::BOOLEAN[],$7::TEXT[]) v(fp,url,source,reason,important,shape)
    ON CONFLICT(run_id,fingerprint) DO UPDATE SET sources=audit_crawl_inventory.sources|EXCLUDED.sources,
    deferred=audit_crawl_inventory.deferred|EXCLUDED.deferred,important=audit_crawl_inventory.important OR EXCLUDED.important,
    url_shape=CASE WHEN COALESCE((audit_crawl_inventory.url_shape->>'complete')::BOOLEAN,FALSE) THEN audit_crawl_inventory.url_shape ELSE EXCLUDED.url_shape END`, runID, fps, urls, sources, reasons, important, shapes)
			if e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE audit_crawl_inventory_state SET url_count=$2,limited=limited OR $3 WHERE run_id=$1`, runID, count, limited); e != nil {
			return e
		}
	}
	return nil
}
