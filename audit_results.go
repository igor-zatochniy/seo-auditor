package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"
)

type resultPersistenceHooks struct {
	before    func(context.Context, pgx.Tx) error
	after     func(context.Context, pgx.Tx, Result) error
	onFailure func(error)
}

func saveResults(ctx context.Context, dbPool *pgxpool.Pool, results <-chan Result, cfg Config, hooks ...resultPersistenceHooks) ResultSummary {
	var hook resultPersistenceHooks
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	query := `
		INSERT INTO audit_results (
			run_id, target_id, safe_url, target_fingerprint, fingerprint_key_id, status_code, scan_status, error_code, error_message,
			is_redirect, redirect_url, title, title_status, description, description_status,
			h1, h1_count, h2_to_h6_status, og_title, og_description, og_image, twitter_card,
			internal_links_count, external_links_count, links_count, canonical_url, is_self_canonical,
			meta_robots, x_robots_tag, robots_allowed, robots_outcome, has_json_ld, has_viewport,
			total_images, images_missing_alt, word_count, duration_ms,
			safe_url_truncated, safe_url_original_length,
			redirect_url_truncated, redirect_url_original_length,
			title_truncated, title_original_length,
			h1_truncated, h1_original_length,
			og_title_truncated, og_title_original_length,
			og_image_truncated, og_image_original_length,
			twitter_card_truncated, twitter_card_original_length,
			canonical_url_truncated, canonical_url_original_length,
			meta_robots_truncated, meta_robots_original_length,
			x_robots_tag_truncated, x_robots_tag_original_length,
			title_char_count, title_width_px, description_char_count, description_width_px,
			description_mobile_status, serp_width_model, serp_width_approximate,
			html_raw_bytes, html_size_complete, googlebot_2mb_status, geo_signals
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, $59, $60, $61, $62, $63, $64, $65, $66, $67, $68)
		ON CONFLICT (run_id, target_id) DO UPDATE SET
			safe_url = EXCLUDED.safe_url,
			target_fingerprint = EXCLUDED.target_fingerprint,
			fingerprint_key_id = EXCLUDED.fingerprint_key_id,
			status_code = EXCLUDED.status_code,
			scan_status = EXCLUDED.scan_status,
			error_code = EXCLUDED.error_code,
			error_message = EXCLUDED.error_message,
			is_redirect = EXCLUDED.is_redirect,
			redirect_url = EXCLUDED.redirect_url,
			title = EXCLUDED.title,
			title_status = EXCLUDED.title_status,
			description = EXCLUDED.description,
			description_status = EXCLUDED.description_status,
			title_char_count = EXCLUDED.title_char_count,
			title_width_px = EXCLUDED.title_width_px,
			description_char_count = EXCLUDED.description_char_count,
			description_width_px = EXCLUDED.description_width_px,
			description_mobile_status = EXCLUDED.description_mobile_status,
			serp_width_model = EXCLUDED.serp_width_model,
			serp_width_approximate = EXCLUDED.serp_width_approximate,
			html_raw_bytes = EXCLUDED.html_raw_bytes,
			html_size_complete = EXCLUDED.html_size_complete,
			googlebot_2mb_status = EXCLUDED.googlebot_2mb_status,
			geo_signals = EXCLUDED.geo_signals,
			h1 = EXCLUDED.h1,
			h1_count = EXCLUDED.h1_count,
			h2_to_h6_status = EXCLUDED.h2_to_h6_status,
			og_title = EXCLUDED.og_title,
			og_description = EXCLUDED.og_description,
			og_image = EXCLUDED.og_image,
			twitter_card = EXCLUDED.twitter_card,
			internal_links_count = EXCLUDED.internal_links_count,
			external_links_count = EXCLUDED.external_links_count,
			links_count = EXCLUDED.links_count,
			canonical_url = EXCLUDED.canonical_url,
			is_self_canonical = EXCLUDED.is_self_canonical,
			meta_robots = EXCLUDED.meta_robots,
			x_robots_tag = EXCLUDED.x_robots_tag,
			robots_allowed = EXCLUDED.robots_allowed,
			robots_outcome = EXCLUDED.robots_outcome,
			has_json_ld = EXCLUDED.has_json_ld,
			has_viewport = EXCLUDED.has_viewport,
			total_images = EXCLUDED.total_images,
			images_missing_alt = EXCLUDED.images_missing_alt,
			word_count = EXCLUDED.word_count,
			duration_ms = EXCLUDED.duration_ms,
			safe_url_truncated = EXCLUDED.safe_url_truncated,
			safe_url_original_length = EXCLUDED.safe_url_original_length,
			redirect_url_truncated = EXCLUDED.redirect_url_truncated,
			redirect_url_original_length = EXCLUDED.redirect_url_original_length,
			title_truncated = EXCLUDED.title_truncated,
			title_original_length = EXCLUDED.title_original_length,
			h1_truncated = EXCLUDED.h1_truncated,
			h1_original_length = EXCLUDED.h1_original_length,
			og_title_truncated = EXCLUDED.og_title_truncated,
			og_title_original_length = EXCLUDED.og_title_original_length,
			og_image_truncated = EXCLUDED.og_image_truncated,
			og_image_original_length = EXCLUDED.og_image_original_length,
			twitter_card_truncated = EXCLUDED.twitter_card_truncated,
			twitter_card_original_length = EXCLUDED.twitter_card_original_length,
			canonical_url_truncated = EXCLUDED.canonical_url_truncated,
			canonical_url_original_length = EXCLUDED.canonical_url_original_length,
			meta_robots_truncated = EXCLUDED.meta_robots_truncated,
			meta_robots_original_length = EXCLUDED.meta_robots_original_length,
			x_robots_tag_truncated = EXCLUDED.x_robots_tag_truncated,
			x_robots_tag_original_length = EXCLUDED.x_robots_tag_original_length,
			created_at = CURRENT_TIMESTAMP;`

	summary := ResultSummary{}
	for res := range results {
		summary.Received++
		d := res.Data
		target := res.Target
		if target.TargetID == 0 {
			slog.Error("Результат SEO-аудиту не має зв'язку зі snapshot target", "url", redactURL(d.URL))
			summary.Failed++
			summary.PersistenceFailures++
			continue
		}
		if target.SafeURL == "" || len(target.Fingerprint) == 0 {
			target = newAuditTarget(targetURLRecord{ID: target.TargetID, URL: d.URL}, d.URL, cfg.TargetFingerprintKey)
		}
		resultFailed := res.Error != nil || d.ScanStatus == scanStatusFailed
		if resultFailed {
			if d.ScanStatus == "" {
				d.ScanStatus = scanStatusFailed
			}
			if d.ErrorCode == "" {
				d.ErrorCode = errorCodeInternal
			}
			if d.ErrorMessage == "" && res.Error != nil {
				d.ErrorMessage = sanitizeError(res.Error)
			}
			d = sanitizeSEODataForStorage(d)
			if res.Error != nil {
				slog.Error(
					"Задача завершилася помилкою",
					"target_id",
					target.TargetID,
					"url",
					target.SafeURL,
					"error_code",
					d.ErrorCode,
					"error",
					d.ErrorMessage,
				)
			}
			summary.Failed++
		}
		if d.ScanStatus == "" {
			d.ScanStatus = scanStatusCompleted
		}
		if d.RobotsOutcome == "" {
			d.RobotsOutcome = robotsOutcomeNotChecked
		}
		d.URL = target.SafeURL
		d = sanitizeSEODataForStorage(d)
		fingerprintKeyID := cfg.TargetFingerprintKeyID
		if fingerprintKeyID == "" {
			fingerprintKeyID = DefaultTargetFingerprintKeyID
		}

		dbWriteCtx, writeCancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
		err := retryDBMutation(
			dbWriteCtx,
			"save_audit_result",
			retryPolicy{maxRetries: cfg.DBMaxRetries, baseDelay: cfg.RetryBaseDelay, maxDelay: cfg.RetryMaxDelay},
			func() error {
				tx, err := dbPool.Begin(dbWriteCtx)
				if err != nil {
					return err
				}
				defer func() {
					_ = tx.Rollback(dbWriteCtx)
				}()
				if hook.before != nil {
					if err := hook.before(dbWriteCtx, tx); err != nil {
						return err
					}
				}

				if _, err := tx.Exec(
					dbWriteCtx,
					query,
					cfg.RunID,
					target.TargetID,
					d.URL,
					target.Fingerprint,
					fingerprintKeyID,
					d.StatusCode,
					d.ScanStatus,
					d.ErrorCode,
					d.ErrorMessage,
					d.IsRedirect,
					d.RedirectURL,
					d.Title,
					d.TitleStatus,
					d.Description,
					d.DescriptionStatus,
					d.H1,
					d.H1Count,
					d.H2ToH6Status,
					d.OGTitle,
					d.OGDescription,
					d.OGImage,
					d.TwitterCard,
					d.InternalLinksCount,
					d.ExternalLinksCount,
					d.LinksCount,
					d.CanonicalURL,
					d.IsSelfCanonical,
					d.MetaRobots,
					d.XRobotsTag,
					d.RobotsAllowed,
					d.RobotsOutcome,
					d.HasJsonLd,
					d.HasViewport,
					d.TotalImages,
					d.ImagesMissingAlt,
					d.WordCount,
					d.Duration.Milliseconds(),
					d.SafeURLTruncated,
					d.SafeURLOriginalLength,
					d.RedirectURLTruncated,
					d.RedirectURLOriginalLength,
					d.TitleTruncated,
					d.TitleOriginalLength,
					d.H1Truncated,
					d.H1OriginalLength,
					d.OGTitleTruncated,
					d.OGTitleOriginalLength,
					d.OGImageTruncated,
					d.OGImageOriginalLength,
					d.TwitterCardTruncated,
					d.TwitterCardOriginalLength,
					d.CanonicalURLTruncated,
					d.CanonicalURLOriginalLength,
					d.MetaRobotsTruncated,
					d.MetaRobotsOriginalLength,
					d.XRobotsTagTruncated,
					d.XRobotsTagOriginalLength,
					d.TitleCharCount,
					d.TitleWidthPX,
					d.DescriptionCharCount,
					d.DescriptionWidthPX,
					d.DescriptionMobileStatus,
					d.SERPWidthModel,
					d.SERPWidthApproximate,
					d.HTMLRawBytes,
					d.HTMLSizeComplete,
					d.Googlebot2MBStatus,
					d.GEO,
				); err != nil {
					return err
				}
				if err := markAuditRunTargetFinished(
					dbWriteCtx,
					tx,
					cfg.RunID,
					target.TargetID,
					effectiveWorkerInstanceID(cfg),
					effectiveOwnerGeneration(cfg),
					finalAuditTargetStatus(d, resultFailed),
					d.ErrorMessage,
				); err != nil {
					return err
				}
				if hook.after != nil {
					if err := hook.after(dbWriteCtx, tx, res); err != nil {
						return err
					}
				}
				return tx.Commit(dbWriteCtx)
			},
		)
		writeCancel()

		if err != nil {
			slog.Error("Не вдалося зберегти результат SEO-аудиту", "target_id", target.TargetID, "url", d.URL, "error", sanitizeError(err))
			if hook.onFailure != nil {
				hook.onFailure(err)
			}
			summary.PersistenceFailures++
			if !resultFailed {
				summary.Failed++
			}
			continue
		}

		summary.Saved++
		if !resultFailed {
			summary.Successful++
		}
		slog.Debug("Результат SEO-аудиту збережено", "target_id", target.TargetID, "url", d.URL)
	}

	return summary
}
