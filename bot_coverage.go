package main

import (
	"context"
	"encoding/json"
	"github.com/igor-zatochniy/seo-auditor/internal/botanalytics"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/jackc/pgx/v5"
)

// Після підтвердження HMAC журналу доповнюємо тільки нові metadata,
// не змінюючи історичні статуси чи перевірку IP.
func updateBotCoverageMetadata(ctx context.Context, tx pgx.Tx, cfg Config, id string, records []botanalytics.Record) error {
	for start := 0; start < len(records); start += 256 {
		ordinals := []int32{}
		old, newFP := [][]byte{}, [][]byte{}
		shapes := []string{}
		for index := start; index < min(start+256, len(records)); index++ {
			u, _, e := normalizeSiteURL(records[index].URL, false)
			if e != nil {
				continue
			}
			raw, _ := json.Marshal(crawler.DescribeURL(u))
			ordinals = append(ordinals, int32(index+1))
			old = append(old, fingerprintURL(cfg.TargetFingerprintKey, records[index].URL))
			newFP = append(newFP, fingerprintURL(cfg.TargetFingerprintKey, u))
			shapes = append(shapes, string(raw))
		}
		if _, e := tx.Exec(ctx, `UPDATE audit_bot_events e SET coverage_fingerprint=v.newfp,url_shape=v.shape::JSONB FROM unnest($2::INT[],$3::BYTEA[],$4::BYTEA[],$5::TEXT[]) v(ordinal,oldfp,newfp,shape)
   WHERE e.import_id=$1 AND e.ordinal=v.ordinal AND e.target_fingerprint=v.oldfp`, id, ordinals, old, newFP, shapes); e != nil {
			return e
		}
	}
	return nil
}
