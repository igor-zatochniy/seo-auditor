package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/seo"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const adaptiveBatchSize = 50

type adaptiveTarget struct {
	Index, Stable, Failures, Priority int
	BaselineRun                       *string
	BaselineTarget                    *int64
}
type conditionalBaseline struct {
	RunID, ETag, LastModified string
	Data                      SEOData
}

func safeHTTPValidator(value string, limit int) string {
	if len(value) > limit {
		return ""
	}
	for _, r := range value {
		if r < 32 || r > 126 {
			return ""
		}
	}
	return value
}

func storeHTTPSnapshot(ctx context.Context, tx pgx.Tx, cfg Config, targetID int64, data SEOData) error {
	if data.StatusCode == nil || *data.StatusCode != 200 || data.ScanStatus != scanStatusCompleted || !data.HTMLSizeComplete {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(encoded) > 128<<10 {
		return nil
	}
	etag, err := sealValidator(cfg.TargetFingerprintKey, safeHTTPValidator(data.HTTPETag, 1024))
	if err != nil {
		return err
	}
	modified, err := sealValidator(cfg.TargetFingerprintKey, safeHTTPValidator(data.HTTPLastModified, 128))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_http_snapshots(run_id,target_id,etag,last_modified,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(run_id,target_id) DO UPDATE SET etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,data=EXCLUDED.data`, cfg.RunID, targetID, etag, modified, json.RawMessage(encoded))
	return err
}

func loadConditionalBaseline(ctx context.Context, pool *pgxpool.Pool, cfg Config, target AuditTarget) (*conditionalBaseline, error) {
	if !cfg.ConditionalRequests || cfg.RenderJavaScript {
		return nil, nil
	}
	keyID := cfg.TargetFingerprintKeyID
	if keyID == "" {
		keyID = DefaultTargetFingerprintKeyID
	}
	var b conditionalBaseline
	var raw []byte
	var productPriceRequired, canonicalSelfRequired bool
	err := pool.QueryRow(ctx, `SELECT b.run_id::TEXT,h.etag,h.last_modified,h.data,
 EXISTS(SELECT 1 FROM audit_rule_runs j WHERE j.run_id=$1 AND j.rules @> '[{"kind":"product_price"}]'::JSONB),
 EXISTS(SELECT 1 FROM audit_rule_runs j WHERE j.run_id=$1 AND j.rules @> '[{"kind":"self_canonical"}]'::JSONB)
	 FROM audit_adaptive_run_targets a JOIN audit_adaptive_targets t USING(schedule_id,source_index)
	 JOIN audit_results b ON b.run_id=a.baseline_run_id AND b.target_id=a.baseline_target_id
	 JOIN audit_http_snapshots h ON h.run_id=b.run_id AND h.target_id=b.target_id
	 WHERE a.run_id=$1 AND a.target_id=$2 AND b.target_fingerprint=$3 AND b.fingerprint_key_id=$4
	 AND b.scan_status='completed' AND b.status_code=200 AND t.stable_checks<16 AND t.stable_checks%5<>4`, cfg.RunID, target.TargetID, target.Fingerprint, keyID).Scan(&b.RunID, &b.ETag, &b.LastModified, &raw, &productPriceRequired, &canonicalSelfRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<10 || json.Unmarshal(raw, &b.Data) != nil || b.Data.SERPWidthModel != seo.SERPWidthModel || !b.Data.HTMLSizeComplete || b.Data.URLSignals == nil || b.Data.URLSignals.Version != crawler.URLSignalsVersion {
		return nil, nil
	}
	if productPriceRequired && (b.Data.StructuredData == nil || b.Data.StructuredData.Version != structureddata.Version || b.Data.StructuredData.Products == nil) {
		return nil, nil
	}
	if canonicalSelfRequired {
		for _, ref := range b.Data.URLSignals.HTTP {
			if ref.Self == nil && ref.Valid && !ref.Truncated {
				return nil, nil
			}
		}
	}
	b.ETag, err = openValidator(cfg.TargetFingerprintKey, b.ETag)
	if err != nil {
		return nil, nil
	}
	b.LastModified, err = openValidator(cfg.TargetFingerprintKey, b.LastModified)
	if err != nil {
		return nil, nil
	}
	if b.ETag == "" && b.LastModified == "" {
		return nil, nil
	}
	return &b, nil
}

func seedAdaptiveTargets(ctx context.Context, tx pgx.Tx, input scheduleInput, source scheduleSource) error {
	if !source.Adaptive {
		return nil
	}
	priorities := make([]int, len(source.URLs))
	for i, raw := range source.URLs {
		priorities[i] = 2
		if u, e := url.Parse(raw); e == nil && (u.Path == "" || u.Path == "/") {
			priorities[i] = 3
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_adaptive_targets(schedule_id,source_index,priority,next_check_at)
	 SELECT $1,n,p,$2 FROM unnest($3::INT[]) WITH ORDINALITY AS x(p,n)`, input.ID, input.NextRunAt, priorities)
	return err
}

func selectAdaptiveTargets(ctx context.Context, tx pgx.Tx, id string, source scheduleSource, force bool) ([]adaptiveTarget, []string, error) {
	rows, err := tx.Query(ctx, `SELECT source_index,stable_checks,consecutive_failures,priority,baseline_run_id::TEXT,baseline_target_id
	 FROM audit_adaptive_targets WHERE schedule_id=$1 AND ($2 OR next_check_at<=CURRENT_TIMESTAMP)
	 ORDER BY next_check_at,priority DESC,source_index LIMIT $3 FOR UPDATE SKIP LOCKED`, id, force, adaptiveBatchSize)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	targets := []adaptiveTarget{}
	urls := []string{}
	for rows.Next() {
		var target adaptiveTarget
		if err = rows.Scan(&target.Index, &target.Stable, &target.Failures, &target.Priority, &target.BaselineRun, &target.BaselineTarget); err != nil {
			return nil, nil, err
		}
		if target.Index < 1 || target.Index > len(source.URLs) {
			return nil, nil, errors.New("adaptive target is outside immutable source")
		}
		targets = append(targets, target)
		urls = append(urls, source.URLs[target.Index-1])
	}
	return targets, urls, rows.Err()
}

func captureAdaptiveBaseline(ctx context.Context, tx pgx.Tx, runID, scheduleID string, targets []adaptiveTarget) error {
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"audit_adaptive_run_targets"}, []string{"run_id", "target_id", "schedule_id", "source_index", "baseline_run_id", "baseline_target_id"}, pgx.CopyFromSlice(len(targets), func(i int) ([]any, error) {
		t := targets[i]
		return []any{runID, int64(i + 1), scheduleID, t.Index, t.BaselineRun, t.BaselineTarget}, nil
	}))
	return err
}

func adaptiveInterval(minHours, maxHours, priority, stable int, failed bool) time.Duration {
	if failed {
		return time.Duration(minHours) * time.Hour
	}
	hours := minHours
	for range min(stable, 10) {
		if hours >= maxHours {
			break
		}
		hours = min(maxHours, hours*2)
	}
	if priority == 1 {
		hours = min(maxHours, hours*2)
	}
	if priority == 3 {
		hours = max(minHours, hours/2)
	}
	return time.Duration(max(minHours, min(maxHours, hours))) * time.Hour
}

func adaptiveDigest(data SEOData) [32]byte {
	value := struct {
		Title, Description, H1, Canonical, MetaRobots, XRobots, Outcome string
		H1Count, Words, Links                                           int
		Signature                                                       any
		Structured                                                      any
	}{data.Title, data.Description, data.H1, data.CanonicalURL, data.MetaRobots, data.XRobotsTag, data.RobotsOutcome, data.H1Count, data.WordCount, data.LinksCount, data.ContentSignature, data.StructuredData}
	raw, _ := json.Marshal(value)
	return sha256.Sum256(raw)
}

func loadAdaptiveResult(ctx context.Context, tx pgx.Tx, run string, target int64) (SEOData, error) {
	var data SEOData
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT status_code,scan_status,error_code,safe_url,COALESCE(h.data,'{}'::JSONB)
	 FROM audit_results r LEFT JOIN audit_http_snapshots h USING(run_id,target_id) WHERE r.run_id=$1 AND r.target_id=$2`, run, target).Scan(&data.StatusCode, &data.ScanStatus, &data.ErrorCode, &data.URL, &raw)
	if err != nil {
		return data, err
	}
	var cached SEOData
	if json.Unmarshal(raw, &cached) == nil && cached.StatusCode != nil {
		return cached, nil
	}
	return data, nil
}

func (s *webServer) compareAdaptiveRun(ctx context.Context, tx pgx.Tx, runID, scheduleID, status string) (string, error) {
	var raw []byte
	var minHours int
	if err := tx.QueryRow(ctx, `SELECT source,interval_hours FROM audit_schedules WHERE id=$1 FOR UPDATE`, scheduleID).Scan(&raw, &minHours); err != nil {
		return "", err
	}
	var source scheduleSource
	if err := json.Unmarshal(raw, &source); err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, `SELECT a.target_id,a.source_index,t.stable_checks,t.consecutive_failures,t.priority,a.baseline_run_id::TEXT,a.baseline_target_id
	 FROM audit_adaptive_run_targets a JOIN audit_adaptive_targets t USING(schedule_id,source_index) WHERE a.run_id=$1 AND a.schedule_id=$2 ORDER BY a.target_id`, runID, scheduleID)
	if err != nil {
		return "", err
	}
	type item struct {
		TargetID int64
		adaptiveTarget
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.TargetID, &i.Index, &i.Stable, &i.Failures, &i.Priority, &i.BaselineRun, &i.BaselineTarget); err != nil {
			rows.Close()
			return "", err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	comparison := "baseline"
	if status != auditRunStatusCompleted && status != auditRunStatusCompletedWithErrors {
		comparison = "interrupted"
	}
	for _, item := range items {
		current, err := loadAdaptiveResult(ctx, tx, runID, item.TargetID)
		missing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !missing {
			return "", err
		}
		failed := missing || current.ScanStatus == scanStatusFailed || current.StatusCode != nil && *current.StatusCode >= 400
		stable, failures := 0, 0
		var baselineRun *string
		var baselineTarget *int64
		if !missing {
			baselineRun = &runID
			target := item.TargetID
			baselineTarget = &target
		}
		if failed {
			failures = min(item.Failures+1, 1000)
		}
		if item.BaselineRun != nil && item.BaselineTarget != nil && !missing {
			previous, e := loadAdaptiveResult(ctx, tx, *item.BaselineRun, *item.BaselineTarget)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return "", e
			}
			if e == nil {
				if status == auditRunStatusCompleted || status == auditRunStatusCompletedWithErrors {
					comparison = "compared"
				}
				if !failed && current.HTMLSizeComplete && previous.HTMLSizeComplete && previous.StatusCode != nil && *previous.StatusCode == 200 && adaptiveDigest(current) == adaptiveDigest(previous) {
					stable = min(item.Stable+1, 16)
				}
				query := strings.Replace(scheduledChangesSQL, "WHERE c.run_id=$1 AND change.detected", "WHERE c.run_id=$1 AND c.target_id=$3 AND change.detected AND change.kind NOT IN ('http_error','request_failed')", 1)
				if _, err = tx.Exec(ctx, query, runID, *item.BaselineRun, item.TargetID); err != nil {
					return "", err
				}
			}
		}
		threshold := source.FailureThreshold
		if threshold < 1 {
			threshold = 2
		}
		if failed && !missing && failures == threshold {
			if _, err = tx.Exec(ctx, `INSERT INTO audit_change_events(run_id,target_id,safe_url,kind,severity,before_value,after_value) VALUES($1,$2,$3,'repeated_failure','critical',$4,$5) ON CONFLICT DO NOTHING`, runID, item.TargetID, current.URL, "Послідовних помилок: "+strconv.Itoa(item.Failures), "Послідовних помилок: "+strconv.Itoa(failures)); err != nil {
				return "", err
			}
		}
		maxHours := max(minHours, source.MaxIntervalHours)
		_, err = tx.Exec(ctx, `UPDATE audit_adaptive_targets SET stable_checks=$3,consecutive_failures=$4,baseline_run_id=$5,baseline_target_id=$6,next_check_at=CURRENT_TIMESTAMP+$7::INTERVAL WHERE schedule_id=$1 AND source_index=$2`, scheduleID, item.Index, stable, failures, baselineRun, baselineTarget, adaptiveInterval(minHours, maxHours, item.Priority, stable, failed).String())
		if err != nil {
			return "", err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE audit_schedules SET next_run_at=COALESCE((SELECT MIN(next_check_at) FROM audit_adaptive_targets WHERE schedule_id=$1),CURRENT_TIMESTAMP+INTERVAL '1 hour') WHERE id=$1`, scheduleID)
	return comparison, err
}
