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
	if status == auditRunStatusCompleted || status == auditRunStatusCompletedWithErrors {
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
CROSS JOIN LATERAL (VALUES
 ('http_error','critical',c.status_code>=400 AND c.status_code IS DISTINCT FROM p.status_code,p.status_code::TEXT,c.status_code::TEXT),
 ('noindex','critical',flags.parsed AND p.geo_signals->'search'->>'google_indexing'='allowed'
   AND c.geo_signals->'search'->>'google_indexing'='blocked',p.meta_robots||' / '||p.x_robots_tag,c.meta_robots||' / '||c.x_robots_tag),
 ('robots_blocked','critical',p.robots_outcome='allowed' AND c.robots_outcome='disallowed',p.robots_outcome,c.robots_outcome),
 ('request_failed','critical',c.scan_status='failed' AND COALESCE(c.status_code,0)<400 AND p.scan_status IS DISTINCT FROM 'failed',p.scan_status,c.error_code),
 ('canonical_changed','warning',flags.parsed AND NOT c.canonical_url_truncated AND NOT p.canonical_url_truncated
   AND COALESCE(c.canonical_url,'')<>COALESCE(p.canonical_url,''),p.canonical_url,c.canonical_url),
 ('redirect_changed','warning',c.is_redirect AND (NOT p.is_redirect OR c.redirect_url IS DISTINCT FROM p.redirect_url OR c.status_code IS DISTINCT FROM p.status_code),p.redirect_url,c.redirect_url),
 ('title_missing','warning',flags.parsed AND COALESCE(p.title,'')<>'' AND COALESCE(c.title,'')='',p.title,c.title),
 ('description_missing','warning',flags.parsed AND COALESCE(p.description,'')<>'' AND COALESCE(c.description,'')='',p.description,c.description),
 ('h1_missing','warning',flags.parsed AND p.h1_count>0 AND c.h1_count=0,p.h1,c.h1),
 ('text_drop','warning',flags.parsed AND c.html_size_complete AND p.html_size_complete AND p.word_count>=100 AND c.word_count::BIGINT*2<p.word_count,p.word_count::TEXT,c.word_count::TEXT)
) change(kind,severity,detected,before_value,after_value)
WHERE c.run_id=$1 AND change.detected
ON CONFLICT (run_id,target_id,kind) DO NOTHING`
