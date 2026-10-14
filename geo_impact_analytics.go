package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type impactDataset struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Engine      string    `json:"engine"`
	PeriodStart string    `json:"period_start"`
	PeriodEnd   string    `json:"period_end"`
	Country     string    `json:"country"`
	Device      string    `json:"device"`
	RowCount    int       `json:"row_count"`
	CreatedAt   time.Time `json:"created_at"`
	ReportID    string    `json:"-"`
	SourceRunID string    `json:"-"`
}

type impactPage struct {
	TargetID              int64    `json:"target_id"`
	URL                   string   `json:"url"`
	HTTPStatus            *int     `json:"http_status"`
	ScanStatus            string   `json:"scan_status"`
	Title                 string   `json:"title"`
	MetaRobots            string   `json:"meta_robots"`
	SelfCanonical         *bool    `json:"self_canonical"`
	RuleFailures          *int     `json:"custom_rule_failures"`
	URLMatch              string   `json:"url_match"`
	GSCImpressions        *int64   `json:"gsc_impressions"`
	GSCClicks             *int64   `json:"gsc_clicks"`
	GSCCTR                *float64 `json:"gsc_ctr"`
	GSCPosition           *float64 `json:"gsc_position"`
	BaselineCTR           *float64 `json:"baseline_ctr"`
	BaselineImpressions   int64    `json:"baseline_impressions"`
	BaselineRows          int      `json:"baseline_rows"`
	OpportunityClicks     *float64 `json:"opportunity_clicks_estimate"`
	OpportunityConfidence string   `json:"opportunity_confidence"`
	OpportunityNote       string   `json:"opportunity_note"`
	GSC_AIImpressions     *int64   `json:"gsc_ai_impressions"`
	GA4Sessions           *int64   `json:"ga4_sessions"`
	AIReferralSessions    *int64   `json:"ai_referral_sessions"`
	VerifiedBotVisits     *int64   `json:"verified_bot_visits"`
	UnverifiedBotVisits   *int64   `json:"unverified_bot_visits"`
}

type impactMetricAggregate struct {
	rows                                         int
	impressions, clicks, sessions                int64
	knownImpressions, knownClicks, knownSessions bool
	ctrWeighted, ctrWeight                       float64
	ctrComplete                                  bool
	positionWeighted, positionWeight             float64
}

func newImpactMetricAggregate() *impactMetricAggregate {
	return &impactMetricAggregate{knownImpressions: true, knownClicks: true, knownSessions: true, ctrComplete: true}
}

func (a *impactMetricAggregate) add(row geo.VisibilityRecord) {
	a.rows++
	if row.Impressions == nil || *row.Impressions < 0 || *row.Impressions > math.MaxInt64-a.impressions {
		a.knownImpressions = false
	} else if a.knownImpressions {
		a.impressions += *row.Impressions
	}
	if row.Clicks == nil || *row.Clicks < 0 || *row.Clicks > math.MaxInt64-a.clicks {
		a.knownClicks = false
	} else if a.knownClicks {
		a.clicks += *row.Clicks
	}
	if row.Sessions == nil || *row.Sessions < 0 || *row.Sessions > math.MaxInt64-a.sessions {
		a.knownSessions = false
	} else if a.knownSessions {
		a.sessions += *row.Sessions
	}
	weight := 1.0
	if row.Impressions != nil && *row.Impressions > 0 {
		weight = float64(*row.Impressions)
	}
	rate := row.CTR
	if row.Clicks != nil && row.Impressions != nil && *row.Impressions > 0 {
		v := float64(*row.Clicks) / float64(*row.Impressions)
		rate = &v
	}
	if rate == nil {
		a.ctrComplete = false
	} else {
		a.ctrWeighted += *rate * weight
		a.ctrWeight += weight
	}
	if row.Position != nil {
		a.positionWeighted += *row.Position * weight
		a.positionWeight += weight
	}
}

func (a *impactMetricAggregate) integer(value int64, known bool) *int64 {
	if a.rows == 0 || !known {
		return nil
	}
	v := value
	return &v
}

func (a *impactMetricAggregate) ctr() *float64 {
	if a.rows == 0 {
		return nil
	}
	if a.knownImpressions && a.knownClicks && a.impressions > 0 {
		v := float64(a.clicks) / float64(a.impressions)
		return &v
	}
	if a.ctrComplete && a.ctrWeight > 0 {
		v := a.ctrWeighted / a.ctrWeight
		return &v
	}
	return nil
}

func (a *impactMetricAggregate) position() *float64 {
	if a.positionWeight == 0 {
		return nil
	}
	v := a.positionWeighted / a.positionWeight
	return &v
}

type impactBaselineBucket struct {
	clicks, impressions     int64
	rows                    int
	ctrWeighted, weight     float64
	complete, ratesComplete bool
}

func positionBucket(position *float64) string {
	if position == nil || *position <= 0 {
		return ""
	}
	switch {
	case *position <= 3:
		return "1-3"
	case *position <= 6:
		return "4-6"
	case *position <= 10:
		return "7-10"
	case *position <= 20:
		return "11-20"
	default:
		return "21+"
	}
}

func (b *impactBaselineBucket) add(row geo.VisibilityRecord) {
	b.rows++
	if row.Impressions == nil || row.Clicks == nil || *row.Impressions < 0 || *row.Clicks < 0 || *row.Clicks > *row.Impressions || *row.Impressions > math.MaxInt64-b.impressions || *row.Clicks > math.MaxInt64-b.clicks {
		b.complete = false
	} else {
		b.impressions += *row.Impressions
		b.clicks += *row.Clicks
	}
	rate := row.CTR
	if row.Impressions != nil && row.Clicks != nil && *row.Impressions > 0 && *row.Clicks >= 0 && *row.Clicks <= *row.Impressions {
		v := float64(*row.Clicks) / float64(*row.Impressions)
		rate = &v
	}
	if rate != nil {
		weight := 1.0
		if row.Impressions != nil && *row.Impressions > 0 {
			weight = float64(*row.Impressions)
		}
		b.ctrWeighted += *rate * weight
		b.weight += weight
	} else {
		b.ratesComplete = false
	}
}

func (b *impactBaselineBucket) ctr() *float64 {
	if b.rows == 0 {
		return nil
	}
	if b.complete && b.impressions > 0 {
		v := float64(b.clicks) / float64(b.impressions)
		return &v
	}
	if b.ratesComplete && b.weight > 0 {
		v := b.ctrWeighted / b.weight
		return &v
	}
	return nil
}

func (s *webServer) geoImpactAnalytics(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeWebError(w, 400, "invalid_limit", "Ліміт сторінки має бути від 1 до 100")
			return
		}
		limit = parsed
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeWebError(w, 400, "invalid_cursor", "Некоректний курсор URL")
			return
		}
		after = parsed
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	report, err := scanGEOReport(tx.QueryRow(ctx, "SELECT "+geoReportColumns+" FROM geo_reports WHERE id=$1", id))
	if err != nil {
		webDBError(w, err)
		return
	}
	datasets, datasetsLimited, err := loadImpactDatasets(ctx, tx, report.Domain)
	if err != nil {
		webDBError(w, err)
		return
	}
	selected, err := selectImpactDatasets(r, datasets)
	if err != nil {
		writeWebError(w, 400, "invalid_dataset", err.Error())
		return
	}
	var baseline *impactDataset
	if selected["gsc_web"] != nil {
		baseline = priorImpactBaseline(datasets, *selected["gsc_web"])
	}
	botImportID := ""
	if err = tx.QueryRow(ctx, `SELECT id::text FROM audit_bot_imports WHERE domain=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, report.Domain).Scan(&botImportID); err != nil && err != pgx.ErrNoRows {
		webDBError(w, err)
		return
	}
	pages, next, err := loadImpactPages(ctx, tx, report.SourceRunID, after, limit)
	if err != nil {
		webDBError(w, err)
		return
	}
	pageByTarget := map[int64]*impactPage{}
	pageByIdentity := map[string]*impactPage{}
	for i := range pages {
		pageByTarget[pages[i].TargetID] = &pages[i].impactPage
		pageByIdentity[impactIdentity(pages[i].TargetFingerprint, pages[i].FingerprintKeyID)] = &pages[i].impactPage
	}
	for _, source := range []string{"gsc_web", "gsc_ai", "ga4"} {
		if dataset := selected[source]; dataset != nil {
			if err = applyVisibilityDataset(ctx, tx, *dataset, report.SourceRunID, source, pageByIdentity, pageByTarget); err != nil {
				webDBError(w, err)
				return
			}
		}
	}
	if baseline != nil {
		baselineRows, e := loadImpactVisibilityRows(ctx, tx, baseline.ID)
		if e != nil {
			webDBError(w, e)
			return
		}
		buckets := map[string]*impactBaselineBucket{}
		for _, row := range baselineRows {
			if row.Scope != "url" && row.Scope != "query_url" {
				continue
			}
			bucket := positionBucket(row.Position)
			if bucket == "" {
				continue
			}
			if buckets[bucket] == nil {
				buckets[bucket] = &impactBaselineBucket{complete: true, ratesComplete: true}
			}
			buckets[bucket].add(row)
		}
		for i := range pages {
			if pages[i].GSCPosition == nil {
				continue
			}
			bucket := buckets[positionBucket(pages[i].GSCPosition)]
			if bucket == nil {
				continue
			}
			pages[i].BaselineCTR = bucket.ctr()
			pages[i].BaselineRows = bucket.rows
			pages[i].BaselineImpressions = bucket.impressions
			actual := pages[i].GSCCTR
			if actual == nil && pages[i].GSCClicks != nil && pages[i].GSCImpressions != nil && *pages[i].GSCImpressions > 0 {
				v := float64(*pages[i].GSCClicks) / float64(*pages[i].GSCImpressions)
				actual = &v
			}
			pages[i].OpportunityClicks, pages[i].OpportunityConfidence, pages[i].OpportunityNote = estimateImpactClicks(pages[i].GSCImpressions, actual, pages[i].BaselineCTR, bucket.rows, bucket.impressions)
		}
	}
	if botImportID != "" {
		if err = applyBotVisits(ctx, tx, botImportID, pageByIdentity); err != nil {
			webDBError(w, err)
			return
		}
	}
	selectedResponse := map[string]any{}
	for _, source := range []string{"gsc_web", "gsc_ai", "ga4"} {
		if selected[source] != nil {
			selectedResponse[source] = selected[source]
		}
	}
	warnings := []string{
		"CTR baseline обчислено з імпортованих CSV, а не повного набору Google; пропущені або обрізані рядки можуть змінити результат.",
		"AI referral sessions визначаються лише за доменом session source у GA4 та не доводять AI-цитування.",
		"GSC generative AI impressions відображаються окремо від GSC Web і не додаються до них.",
	}
	if datasetsLimited {
		warnings = append(warnings, "Для вибору показано останні 300 наборів даних; старіші імпорти не включено до цього списку.")
	}
	if baseline == nil {
		warnings = append(warnings, "Немає попереднього неперекривного GSC Web імпорту з такими самими країною та пристроєм; click opportunity не розраховано.")
	}
	if botImportID == "" {
		warnings = append(warnings, "Імпорт журналів ботів відсутній; verified bot visits не перевірялися.")
	}
	writeWebJSON(w, 200, map[string]any{
		"report": report, "datasets": datasets, "selected": selectedResponse,
		"baseline": baseline, "bot_import_id": botImportID, "datasets_limited": datasetsLimited,
		"items": pages, "next": next, "warnings": warnings,
	})
}

func estimateImpactClicks(impressions *int64, actualCTR, baselineCTR *float64, baselineRows int, baselineImpressions int64) (*float64, string, string) {
	if impressions == nil || *impressions <= 0 || actualCTR == nil || baselineCTR == nil || baselineRows == 0 {
		return nil, "unknown", "Немає достатніх спостережень для порівняння CTR."
	}
	reserve := float64(*impressions) * (*baselineCTR - *actualCTR)
	if reserve < 0 {
		reserve = 0
	}
	switch {
	case baselineRows >= 30 && baselineImpressions >= 10000:
		return &reserve, "high", "Орієнтир з великою імпортованою baseline-вибіркою; не прогноз ефекту виправлень."
	case baselineRows >= 10 && baselineImpressions >= 1000:
		return &reserve, "medium", "Орієнтир за попереднім GSC-імпортом того самого сайту, пристрою, країни та діапазону позиції; не прогноз ефекту виправлень."
	default:
		return &reserve, "low", "Мала вибірка baseline; оцінка має низьку впевненість і не є прогнозом."
	}
}

type impactPageDB struct {
	impactPage
	TargetFingerprint []byte `json:"-"`
	FingerprintKeyID  string `json:"-"`
}

func loadImpactPages(ctx context.Context, tx pgx.Tx, runID string, after int64, limit int) ([]impactPageDB, int64, error) {
	rows, err := tx.Query(ctx, `SELECT a.target_id,a.safe_url,a.status_code,a.scan_status,a.title,a.meta_robots,a.is_self_canonical,
 CASE WHEN EXISTS(SELECT 1 FROM audit_rule_runs rr WHERE rr.run_id=a.run_id)
      THEN COALESCE((SELECT COUNT(*)::int FROM audit_rule_results ar WHERE ar.run_id=a.run_id AND ar.target_id=a.target_id AND ar.state='fail'),0)
      ELSE NULL END,
 a.target_fingerprint,a.fingerprint_key_id
 FROM audit_results a WHERE a.run_id=$1 AND a.target_id>$2 ORDER BY a.target_id LIMIT $3`, runID, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []impactPageDB{}
	for rows.Next() {
		var item impactPageDB
		var status pgtype.Int4
		var canonical pgtype.Bool
		var failures pgtype.Int4
		if err = rows.Scan(&item.TargetID, &item.URL, &status, &item.ScanStatus, &item.Title, &item.MetaRobots, &canonical, &failures, &item.TargetFingerprint, &item.FingerprintKeyID); err != nil {
			return nil, 0, err
		}
		if status.Valid {
			v := int(status.Int32)
			item.HTTPStatus = &v
		}
		if canonical.Valid {
			v := canonical.Bool
			item.SelfCanonical = &v
		}
		if failures.Valid {
			v := int(failures.Int32)
			item.RuleFailures = &v
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(items) > limit {
		next = items[limit-1].TargetID
		items = items[:limit]
	}
	return items, next, nil
}

func impactIdentity(fingerprint []byte, keyID string) string {
	return keyID + "\x00" + hex.EncodeToString(fingerprint)
}

func loadImpactDatasets(ctx context.Context, tx pgx.Tx, domain string) ([]impactDataset, bool, error) {
	rows, err := tx.Query(ctx, `SELECT i.id::text,i.source,i.engine,i.period_start::text,i.period_end::text,i.country,i.device,i.row_count,i.created_at,
 i.report_id::text,g.source_run_id::text
 FROM geo_visibility_imports i JOIN geo_reports g ON g.id=i.report_id
 WHERE g.domain=$1 AND i.source IN ('gsc_web','gsc_ai','ga4')
 ORDER BY i.period_end DESC,i.created_at DESC,i.id DESC LIMIT 301`, domain)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []impactDataset{}
	for rows.Next() {
		var d impactDataset
		if err = rows.Scan(&d.ID, &d.Source, &d.Engine, &d.PeriodStart, &d.PeriodEnd, &d.Country, &d.Device, &d.RowCount, &d.CreatedAt, &d.ReportID, &d.SourceRunID); err != nil {
			return nil, false, err
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	limited := len(items) > 300
	if limited {
		items = items[:300]
	}
	return items, limited, nil
}

func selectImpactDatasets(r *http.Request, datasets []impactDataset) (map[string]*impactDataset, error) {
	selected := map[string]*impactDataset{}
	for _, source := range []string{"gsc_web", "gsc_ai", "ga4"} {
		requested := r.URL.Query().Get(source)
		for i := range datasets {
			d := &datasets[i]
			if d.Source != source {
				continue
			}
			if requested == "" {
				selected[source] = d
				break
			}
			if d.ID == requested {
				selected[source] = d
				break
			}
		}
		if requested != "" && selected[source] == nil {
			return nil, fmt.Errorf("обраний набір %s відсутній для домену цього звіту", source)
		}
	}
	return selected, nil
}

func priorImpactBaseline(datasets []impactDataset, current impactDataset) *impactDataset {
	var best *impactDataset
	for i := range datasets {
		d := &datasets[i]
		if d.Source != "gsc_web" || d.ID == current.ID || d.Country != current.Country || d.Device != current.Device || d.PeriodEnd >= current.PeriodStart {
			continue
		}
		if best == nil || d.PeriodEnd > best.PeriodEnd || (d.PeriodEnd == best.PeriodEnd && d.CreatedAt.After(best.CreatedAt)) {
			best = d
		}
	}
	return best
}

func loadImpactVisibilityRows(ctx context.Context, tx pgx.Tx, importID string) ([]geo.VisibilityRecord, error) {
	rows, err := tx.Query(ctx, `SELECT observation FROM geo_visibility_rows WHERE import_id=$1 ORDER BY ordinal`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []geo.VisibilityRecord{}
	for rows.Next() {
		var item geo.VisibilityRecord
		if err = rows.Scan(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func applyVisibilityDataset(ctx context.Context, tx pgx.Tx, dataset impactDataset, currentRun, source string, byIdentity map[string]*impactPage, byTarget map[int64]*impactPage) error {
	rows, err := loadImpactVisibilityRows(ctx, tx, dataset.ID)
	if err != nil {
		return err
	}
	aggregates := map[int64]*impactMetricAggregate{}
	aiSessions := map[int64]int64{}
	aiSeen := map[int64]bool{}
	for _, record := range rows {
		var page *impactPage
		if record.MatchedFingerprint != "" && record.MatchKeyID != "" {
			page = byIdentity[record.MatchKeyID+"\x00"+record.MatchedFingerprint]
		}
		if page == nil && dataset.SourceRunID == currentRun && record.TargetID != nil {
			page = byTarget[*record.TargetID]
			if page != nil && page.URLMatch == "" {
				page.URLMatch = "exact"
			}
		}
		if page == nil {
			continue
		}
		if record.URLMatch == "exact" {
			page.URLMatch = "exact"
		} else if record.URLMatch == "canonical_match" && page.URLMatch != "exact" {
			page.URLMatch = "canonical_match"
		}
		agg := aggregates[page.TargetID]
		if agg == nil {
			agg = newImpactMetricAggregate()
			aggregates[page.TargetID] = agg
		}
		agg.add(record)
		if source == "ga4" && record.AIReferral {
			aiSeen[page.TargetID] = true
			if record.Sessions != nil && *record.Sessions >= 0 && *record.Sessions <= math.MaxInt64-aiSessions[page.TargetID] {
				aiSessions[page.TargetID] += *record.Sessions
			}
		}
	}
	for targetID, agg := range aggregates {
		page := byTarget[targetID]
		switch source {
		case "gsc_web":
			page.GSCImpressions = agg.integer(agg.impressions, agg.knownImpressions)
			page.GSCClicks = agg.integer(agg.clicks, agg.knownClicks)
			page.GSCCTR = agg.ctr()
			page.GSCPosition = agg.position()
		case "gsc_ai":
			page.GSC_AIImpressions = agg.integer(agg.impressions, agg.knownImpressions)
		case "ga4":
			page.GA4Sessions = agg.integer(agg.sessions, agg.knownSessions)
			if aiSeen[targetID] {
				ai := aiSessions[targetID]
				page.AIReferralSessions = &ai
			}
		}
	}
	return nil
}

func applyBotVisits(ctx context.Context, tx pgx.Tx, importID string, pages map[string]*impactPage) error {
	rows, err := tx.Query(ctx, `SELECT e.target_fingerprint,i.fingerprint_key_id,e.verification,COUNT(*)
 FROM audit_bot_events e JOIN audit_bot_imports i ON i.id=e.import_id
 WHERE e.import_id=$1 GROUP BY e.target_fingerprint,i.fingerprint_key_id,e.verification`, importID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var fingerprint []byte
		var keyID, verification string
		var count int64
		if err = rows.Scan(&fingerprint, &keyID, &verification, &count); err != nil {
			return err
		}
		page := pages[impactIdentity(fingerprint, keyID)]
		if page == nil {
			continue
		}
		switch verification {
		case "matched_range":
			if page.VerifiedBotVisits == nil {
				page.VerifiedBotVisits = new(int64)
			}
			*page.VerifiedBotVisits += count
		case "unverified":
			if page.UnverifiedBotVisits == nil {
				page.UnverifiedBotVisits = new(int64)
			}
			*page.UnverifiedBotVisits += count
		}
	}
	return rows.Err()
}
