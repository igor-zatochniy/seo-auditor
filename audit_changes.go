package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Порівнюємо HTML відповіді; DOM після JavaScript залишається у звіті самого аудиту.
func (s *webServer) compareScheduledRun(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var runID, scheduleID, status string
	err = tx.QueryRow(ctx, `SELECT sr.run_id::TEXT,sr.schedule_id::TEXT,r.status
		FROM audit_schedule_runs sr JOIN audit_runs r ON r.id=sr.run_id
		WHERE sr.comparison_status='pending' AND r.status<>'running'
		ORDER BY sr.scheduled_at,sr.run_id LIMIT 1 FOR UPDATE OF sr SKIP LOCKED`).Scan(&runID, &scheduleID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	comparison := "interrupted"
	var baseline *string
	var adaptive bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE((source->>'adaptive')::BOOLEAN,FALSE) FROM audit_schedules WHERE id=$1`, scheduleID).Scan(&adaptive); err != nil {
		return false, err
	}
	if adaptive {
		comparison, err = s.compareAdaptiveRun(ctx, tx, runID, scheduleID, status)
		if err != nil {
			return false, err
		}
		if comparison == "interrupted" {
			if _, err = tx.Exec(ctx, `INSERT INTO audit_change_events(run_id,target_id,safe_url,kind,severity,after_value) VALUES($1,0,'','audit_interrupted','critical',$2) ON CONFLICT DO NOTHING`, runID, status); err != nil {
				return false, err
			}
		}
	} else if status == auditRunStatusCompleted || status == auditRunStatusCompletedWithErrors {
		comparison = "baseline"
		var previous string
		err = tx.QueryRow(ctx, `SELECT p.run_id::TEXT FROM audit_schedule_runs p
		 JOIN audit_runs r ON r.id=p.run_id JOIN audit_schedule_runs c ON c.run_id=$1
		 WHERE p.schedule_id=$2 AND p.key_check=c.key_check
		 AND (p.scheduled_at,p.run_id)<(c.scheduled_at,c.run_id)
		 AND p.comparison_status IN ('baseline','compared') AND r.status IN ('completed','completed_with_errors')
		 ORDER BY p.scheduled_at DESC,p.run_id DESC LIMIT 1`, runID, scheduleID).Scan(&previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if err == nil {
			baseline, comparison = &previous, "compared"
			if _, err = tx.Exec(ctx, scheduledChangesSQL, runID, previous); err != nil {
				return false, err
			}
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO audit_change_events(run_id,target_id,safe_url,kind,severity,after_value)
		 VALUES($1,0,'','audit_interrupted','critical',$2) ON CONFLICT DO NOTHING`, runID, status)
		if err != nil {
			return false, err
		}
	}
	// Планові запуски не відновлюються: після terminal state залишаємо
	// вихідні URL лише у конфігурації розкладу, а не в кожному snapshot.
	if _, err = tx.Exec(ctx, `UPDATE audit_run_targets SET request_url='',request_url_cleared_at=CURRENT_TIMESTAMP
	 WHERE run_id=$1 AND request_url<>''`, runID); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE audit_schedule_runs SET comparison_status=$2,baseline_run_id=$3,compared_at=CURRENT_TIMESTAMP,
	 change_count=(SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1) WHERE run_id=$1`, runID, comparison, baseline)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Ідентичність визначає HMAC, а не замаскований URL. Повтор після невизначеного
// COMMIT не дублює події: події та стан порівняння мають одну транзакцію.
const scheduledChangesSQL = `
INSERT INTO audit_change_events(run_id,target_id,safe_url,kind,severity,before_value,after_value)
SELECT c.run_id,c.target_id,c.safe_url,change.kind,change.severity,
       LEFT(COALESCE(change.before_value,''),2048),LEFT(COALESCE(change.after_value,''),2048)
FROM audit_results c
LEFT JOIN LATERAL (
 SELECT p.* FROM audit_results p WHERE p.run_id=$2 AND p.target_fingerprint=c.target_fingerprint
 AND p.fingerprint_key_id=c.fingerprint_key_id ORDER BY p.target_id LIMIT 1
) p ON TRUE
CROSS JOIN LATERAL (SELECT c.scan_status='completed' AND c.status_code=200
 AND p.scan_status='completed' AND p.status_code=200 AS parsed) flags
LEFT JOIN LATERAL (
 SELECT BOOL_OR(ref.kind='html' AND NOT (ref.current_fingerprints ?| ARRAY(SELECT jsonb_array_elements_text(ref.previous_fingerprints)))) AS canonical_changed,
        BOOL_OR(ref.kind='redirect' AND NOT (ref.current_fingerprints ?| ARRAY(SELECT jsonb_array_elements_text(ref.previous_fingerprints)))) AS redirect_changed
 FROM (
  SELECT v.*,
   CASE WHEN jsonb_typeof(v.current_ref->'fingerprints')='array' THEN v.current_ref->'fingerprints' ELSE '[]'::JSONB END AS current_fingerprints,
   CASE WHEN jsonb_typeof(v.previous_ref->'fingerprints')='array' THEN v.previous_ref->'fingerprints' ELSE '[]'::JSONB END AS previous_fingerprints
  FROM (VALUES ('html',c.url_signals->'html',p.url_signals->'html'),
               ('redirect',c.url_signals->'redirect',p.url_signals->'redirect')) v(kind,current_ref,previous_ref)
 ) ref
 WHERE c.url_signals->>'version'='url-signals-v1' AND p.url_signals->>'version'=c.url_signals->>'version'
 AND c.url_signals->>'complete'='true' AND p.url_signals->>'complete'='true'
 AND ref.current_ref->>'valid'='true' AND ref.previous_ref->>'valid'='true'
 AND COALESCE(ref.current_ref->>'truncated','false')='false' AND COALESCE(ref.previous_ref->>'truncated','false')='false'
 AND jsonb_array_length(ref.current_fingerprints)>0 AND jsonb_array_length(ref.previous_fingerprints)>0
) identities ON TRUE
CROSS JOIN LATERAL (VALUES
 ('http_error','critical',c.status_code>=400 AND c.status_code IS DISTINCT FROM p.status_code,p.status_code::TEXT,c.status_code::TEXT),
 ('noindex','critical',flags.parsed AND p.geo_signals->'search'->>'google_indexing'='allowed'
   AND c.geo_signals->'search'->>'google_indexing'='blocked',p.meta_robots||' / '||p.x_robots_tag,c.meta_robots||' / '||c.x_robots_tag),
 ('robots_blocked','critical',p.robots_outcome='allowed' AND c.robots_outcome='disallowed',p.robots_outcome,c.robots_outcome),
 ('request_failed','critical',c.scan_status='failed' AND COALESCE(c.status_code,0)<400 AND p.scan_status IS DISTINCT FROM 'failed',p.scan_status,c.error_code),
 ('canonical_changed','warning',flags.parsed AND NOT c.canonical_url_truncated AND NOT p.canonical_url_truncated
   AND COALESCE(c.url_signals->>'complete','true')='true' AND COALESCE(p.url_signals->>'complete','true')='true'
   AND (COALESCE(c.canonical_url,'')<>COALESCE(p.canonical_url,'') OR identities.canonical_changed),p.canonical_url,c.canonical_url),
 ('redirect_changed','warning',c.is_redirect AND (NOT p.is_redirect OR c.status_code IS DISTINCT FROM p.status_code
   OR (NOT c.redirect_url_truncated AND NOT p.redirect_url_truncated
     AND COALESCE(c.url_signals->>'complete','true')='true' AND COALESCE(p.url_signals->>'complete','true')='true'
     AND (c.redirect_url IS DISTINCT FROM p.redirect_url OR identities.redirect_changed))),p.redirect_url,c.redirect_url),
 ('title_missing','warning',flags.parsed AND COALESCE(p.title,'')<>'' AND COALESCE(c.title,'')='',p.title,c.title),
 ('description_missing','warning',flags.parsed AND COALESCE(p.description,'')<>'' AND COALESCE(c.description,'')='',p.description,c.description),
 ('h1_missing','warning',flags.parsed AND p.h1_count>0 AND c.h1_count=0,p.h1,c.h1),
 ('text_drop','warning',flags.parsed AND c.html_size_complete AND p.html_size_complete AND p.word_count>=100 AND c.word_count::BIGINT*2<p.word_count,p.word_count::TEXT,c.word_count::TEXT)
) change(kind,severity,detected,before_value,after_value)
WHERE c.run_id=$1 AND change.detected
ON CONFLICT (run_id,target_id,kind) DO NOTHING`
