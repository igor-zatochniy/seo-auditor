package main

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type aggregateMetric struct{ label, expression string }
type aggregateGroup struct {
	label   string
	metrics []aggregateMetric
}

func countWhere(label, predicate string) aggregateMetric {
	return aggregateMetric{label, "COUNT(*) FILTER (WHERE " + predicate + ")::DOUBLE PRECISION"}
}
func parsedCount(label, predicate string) aggregateMetric {
	return countWhere(label, parsedPagePredicate+" AND ("+predicate+")")
}

var reportAggregateGroups = []aggregateGroup{
	{"HTTP", []aggregateMetric{countWhere("2xx", "r.status_code BETWEEN 200 AND 299"), countWhere("3xx", "r.status_code BETWEEN 300 AND 399"), countWhere("4xx", "r.status_code BETWEEN 400 AND 499"), countWhere("5xx", "r.status_code BETWEEN 500 AND 599"), countWhere("Без відповіді", "r.status_code IS NULL")}},
	{"Результати", []aggregateMetric{countWhere("Завершено", "r.scan_status='completed'"), countWhere("Редиректи", "r.scan_status='redirect'"), countWhere("Robots blocked", "r.scan_status='blocked_by_robots'"), countWhere("Помилки", "r.scan_status='failed'")}},
	{"Title / SERP width", []aggregateMetric{parsedCount("Recommended (≤580 px)", "r.title_status='Recommended'"), parsedCount("Borderline (581–600 px)", "r.title_status='Borderline'"), parsedCount("High truncation risk (>600 px)", "r.title_status='High truncation risk'"), parsedCount("Відсутній", "r.title_status='Missing'"), parsedCount("Не виміряно / legacy", "r.title_width_px IS NULL")}},
	{"Description / Desktop", []aggregateMetric{parsedCount("Safe (≤920 px)", "r.description_status='Safe'"), parsedCount("May truncate (>920 px)", "r.description_status='May truncate'"), parsedCount("Відсутній", "r.description_status='Missing'"), parsedCount("Не виміряно / legacy", "r.description_width_px IS NULL")}},
	{"Description / Mobile", []aggregateMetric{parsedCount("Safe (≤680 px)", "r.description_mobile_status='Safe'"), parsedCount("May truncate (681–920 px)", "r.description_mobile_status='May truncate'"), parsedCount("Likely truncate (>920 px)", "r.description_mobile_status='Likely truncate'")}},
	{"H1", []aggregateMetric{parsedCount("Відсутній", "r.h1_count=0"), parsedCount("Один", "r.h1_count=1"), parsedCount("Декілька", "r.h1_count>1")}},
	{"Canonical", []aggregateMetric{parsedCount("Відсутній", "COALESCE(r.canonical_url,'')=''"), parsedCount("Self", "r.is_self_canonical"), parsedCount("Non-self", "COALESCE(r.canonical_url,'')<>'' AND NOT r.is_self_canonical")}},
	{"Robots / індексація", []aggregateMetric{countWhere("Дозволено", "r.robots_outcome='allowed'"), countWhere("Заборонено", "r.robots_outcome='disallowed'"), countWhere("Noindex / none (будь-який scope)", noindexPredicate)}},
	{"Технічні сигнали", []aggregateMetric{parsedCount("JSON-LD є", "r.has_json_ld"), parsedCount("JSON-LD немає", "NOT r.has_json_ld"), parsedCount("Viewport є", "r.has_viewport"), parsedCount("Viewport немає", "NOT r.has_viewport")}},
	{"Зображення", []aggregateMetric{parsedCount("Сторінки без alt", "r.images_missing_alt>0"), {"Без alt", "COALESCE(SUM(r.images_missing_alt),0)::DOUBLE PRECISION"}, {"Усього", "COALESCE(SUM(r.total_images),0)::DOUBLE PRECISION"}}},
	{"Час, ms", []aggregateMetric{{"Середній", "COALESCE(AVG(r.duration_ms),0)"}, {"Медіана", "COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY r.duration_ms),0)"}, {"p95", "COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY r.duration_ms),0)"}}},
	{"Контент, слова", []aggregateMetric{{"Середнє", "COALESCE(AVG(r.word_count) FILTER (WHERE " + parsedPagePredicate + "),0)"}, {"Медіана", "COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY r.word_count) FILTER (WHERE " + parsedPagePredicate + "),0)"}, parsedCount("0–99", "r.word_count<100"), parsedCount("100–499", "r.word_count BETWEEN 100 AND 499"), parsedCount("500–1499", "r.word_count BETWEEN 500 AND 1499"), parsedCount("1500+", "r.word_count>=1500")}},
}

func init() {
	reportAggregateGroups = append(reportAggregateGroups,
		aggregateGroup{"HTML / Googlebot", []aggregateMetric{countWhere("Cutoff risk (≥2 MiB)", "r.googlebot_2mb_status='Googlebot cutoff risk'"), countWhere("Менше 2 MiB", "r.googlebot_2mb_status='OK'"), countWhere("Неповне читання", "r.html_raw_bytes IS NOT NULL AND NOT r.html_size_complete"), countWhere("Не виміряно", "r.html_raw_bytes IS NULL")}},
		aggregateGroup{"Граф сайту", []aggregateMetric{countWhere("Понад 3 кліки", "sc.graph_ready AND n.crawl_depth>3"), countWhere("Orphan candidates", "sc.graph_ready AND n.orphan_candidate"), countWhere("Сторінки з битими посиланнями", "sc.graph_ready AND n.broken_internal_links>0"), countWhere("Сторінки з внутрішніми редиректами", "sc.graph_ready AND n.redirecting_internal_links>0"), countWhere("Ліміт збору посилань", "n.links_truncated")}},
	)
}

type reportQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadWebAnalytics(ctx context.Context, pool reportQuerier, id string) (webAnalytics, error) {
	a := webAnalytics{Groups: make([]analyticsGroup, 0, len(reportAggregateGroups)+1)}
	expressions := []string{"COUNT(*)", "COUNT(*) FILTER (WHERE " + parsedPagePredicate + ")"}
	dest := []any{&a.Total, &a.Parsed}
	for _, group := range reportAggregateGroups {
		g := analyticsGroup{Label: group.label, Buckets: make([]analyticsBucket, len(group.metrics))}
		for i, metric := range group.metrics {
			g.Buckets[i].Label = metric.label
			expressions = append(expressions, metric.expression)
			dest = append(dest, &g.Buckets[i].Value)
		}
		a.Groups = append(a.Groups, g)
	}
	if err := pool.QueryRow(ctx, "SELECT "+strings.Join(expressions, ",")+" FROM audit_results r LEFT JOIN audit_site_nodes n USING(run_id,target_id) LEFT JOIN audit_site_crawls sc USING(run_id) WHERE r.run_id=$1", id).Scan(dest...); err != nil {
		return a, err
	}
	rows, err := pool.Query(ctx, "SELECT error_code,COUNT(*)::DOUBLE PRECISION FROM audit_results WHERE run_id=$1 AND error_code<>'' GROUP BY error_code ORDER BY COUNT(*) DESC,error_code LIMIT 50", id)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	errors := analyticsGroup{Label: "Коди помилок", Buckets: []analyticsBucket{}}
	for rows.Next() {
		var b analyticsBucket
		if err := rows.Scan(&b.Label, &b.Value); err != nil {
			return a, err
		}
		errors.Buckets = append(errors.Buckets, b)
	}
	a.Groups = append(a.Groups, errors)
	for i := range a.Groups {
		a.Groups[i].Maximum = 1
		for _, bucket := range a.Groups[i].Buckets {
			a.Groups[i].Maximum = max(a.Groups[i].Maximum, bucket.Value)
		}
	}
	return a, rows.Err()
}
