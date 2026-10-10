package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
	"github.com/jackc/pgx/v5/pgxpool"
)

func secureInternationalEvidence(e *international.Evidence, canonical string, cfg Config) *international.Evidence {
	if e == nil {
		return nil
	}
	v := *e
	v.Links = append([]international.Alternate{}, e.Links...)
	if len(v.Links) > international.MaxAlternates {
		v.Links = v.Links[:international.MaxAlternates]
		v.Complete = false
	}
	for i := range v.Links {
		a := &v.Links[i]
		a.LanguageValid = international.ValidLanguage(a.Language)
		if a.ValidURL {
			u, err := normalizeTargetURL(a.URL, cfg.AllowPrivateTargets)
			if err == nil {
				a.Fingerprint = hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, u))
			} else {
				a.ValidURL = false
				a.Fingerprint = ""
			}
		}
		a.URL = redactURL(a.URL)
		if len(a.URL) > 2048 {
			a.URL = strings.ToValidUTF8(a.URL[:2048], "")
			v.Complete = false
		}
		a.Language = sanitizeGEOTextForStorage(a.Language, 64)
	}
	for {
		b, _ := json.Marshal(v)
		if len(b) <= 140000 || len(v.Links) == 0 {
			break
		}
		v.Links = v.Links[:len(v.Links)-1]
		v.Complete = false
	}
	if canonical != "" {
		if u, err := normalizeTargetURL(canonical, cfg.AllowPrivateTargets); err == nil {
			v.CanonicalFingerprint = hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, u))
		}
	}
	return &v
}

func saveSitemapHreflang(ctx context.Context, pool *pgxpool.Pool, cfg Config, site *siteCrawl, records map[string]*international.Evidence) error {
	keys := make([]string, 0, len(records))
	for u := range records {
		keys = append(keys, u)
	}
	sort.Strings(keys)
	for len(keys) > 0 {
		batch := keys[:min(len(keys), 50)]
		keys = keys[len(batch):]
		if err := withDBMutationRetry(ctx, cfg, "save_sitemap_hreflang", func(qctx context.Context) error {
			tx, err := pool.Begin(qctx)
			if err != nil {
				return err
			}
			defer rollbackSiteTransaction(tx, cfg)
			if err = lockSiteOwner(qctx, tx, cfg); err != nil {
				return err
			}
			for _, raw := range batch {
				u, origin, err := normalizeSiteURL(raw, cfg.AllowPrivateTargets)
				if err != nil || origin != site.Origin {
					continue
				}
				e := secureInternationalEvidence(records[raw], "", cfg)
				b, err := json.Marshal(e)
				if err != nil {
					return err
				}
				_, err = tx.Exec(qctx, `INSERT INTO audit_sitemap_hreflang(run_id,page_fingerprint,evidence) VALUES($1,$2,$3)
				ON CONFLICT(run_id,page_fingerprint) DO UPDATE SET evidence=(
				WITH links AS (SELECT DISTINCT value FROM jsonb_array_elements((audit_sitemap_hreflang.evidence->'links')||(EXCLUDED.evidence->'links'))),
				bounded AS (SELECT value,ROW_NUMBER() OVER(ORDER BY value) AS ordinal,SUM(octet_length(value::TEXT)+2) OVER(ORDER BY value) AS weight FROM links)
				SELECT jsonb_build_object('links',COALESCE(jsonb_agg(value ORDER BY value) FILTER(WHERE ordinal<=64 AND weight<=140000),'[]'::JSONB),
				'complete',(audit_sitemap_hreflang.evidence->>'complete')::BOOLEAN AND (EXCLUDED.evidence->>'complete')::BOOLEAN AND COALESCE(bool_and(ordinal<=64 AND weight<=140000),TRUE)) FROM bounded)`, cfg.RunID, fingerprintURL(cfg.TargetFingerprintKey, u), b)
				if err != nil {
					return err
				}
			}
			return tx.Commit(qctx)
		}); err != nil {
			return err
		}
	}
	return nil
}
