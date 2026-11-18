package main

import (
	"context"
	"fmt"
	"github.com/igor-zatochniy/seo-auditor/internal/duplicates"
	"github.com/igor-zatochniy/seo-auditor/internal/international"
	"html/template"
)

type reportDiagnostics struct {
	Coverage   *reportCoverage
	Duplicates duplicates.Summary
	Clusters   []duplicates.Cluster
}

func loadReportDiagnostics(ctx context.Context, q reportQuerier, id string) (reportDiagnostics, error) {
	c, err := loadReportCoverage(ctx, q, id)
	if err != nil {
		return reportDiagnostics{}, err
	}
	summary, clusters, err := loadDuplicateAnalysis(ctx, q, id)
	return reportDiagnostics{c, summary, clusters}, err
}

func internationalReportDetails(p international.Page) []reportDetail {
	const group = "Міжнародне SEO / Response HTML і sitemap"
	details := []reportDetail{{"Статус hreflang", map[string]string{"not_checked": "Не перевірено", "ok": "Перевірено", "review": "Потребує перевірки", "error": "Знайдено порушення"}[p.Status], group}}
	observed := func(v *bool) string {
		if v == nil {
			return "не перевірено"
		}
		if *v {
			return "так"
		}
		return "ні"
	}
	for _, source := range []struct {
		name     string
		evidence *international.Evidence
	}{{"HTML", p.HTML}, {"Sitemap", p.Sitemap}} {
		if source.evidence == nil {
			continue
		}
		details = append(details, reportDetail{source.name + ": збір повний", fmt.Sprint(source.evidence.Complete), group})
		for _, link := range source.evidence.Links {
			o := p.Observations[link.Fingerprint]
			http := "не перевірено"
			if o.HTTP != nil {
				http = fmt.Sprint(*o.HTTP)
			}
			details = append(details, reportDetail{source.name + " / " + redactText(link.Language), fmt.Sprintf("%s\nHTTP: %s; взаємність: %s; self-canonical: %s", redactURL(link.URL), http, observed(o.Reciprocal), observed(o.CanonicalSelf)), group})
		}
	}
	for _, issue := range p.Issues {
		details = append(details, reportDetail{redactText(issue.Code) + " / " + issue.Source, redactText(issue.Message) + " " + redactURL(issue.URL), group})
	}
	return details
}

var reportDiagnosticsTemplate = template.Must(template.New("report-diagnostics").Parse(`
{{with .Coverage}}<section id="crawl-coverage"><h2>Повнота обходу</h2>
<p>{{.Root}} · ліміт {{.MaxPages}} сторінок · глибина discovery {{.MaxDepth}}.</p>
<p>Відомих URL: <strong>{{.Known}}</strong>; HTTP перевірено: <strong>{{.Checked}}</strong>.</p>
<p>Ймовірні ресурси за розширенням, не перевірені як HTML: <strong>{{.ResourceHints}}</strong>. Непідтриманий Content-Type у відповідях: <strong>{{.UnsupportedTypes}}</strong>; це не означає HTTP-помилку сайту. Розширення URL — лише евристика, не підтвердження типу відповіді.</p>
{{if .SitemapKnown}}<p>Перевірено <strong>{{.SitemapChecked}} з {{.SitemapKnown}}</strong> відомих URL sitemap: <strong>{{printf "%.1f" .SitemapPercent}}%</strong>. Sitemap: {{.SitemapState}}. Це частка від зібраного переліку, не гарантія повного охоплення сайту.</p>{{end}}
{{if .Partial}}<p><strong>Неповне спостережене покриття.</strong> Completed означає завершення вибраних завдань, а не перевірку всього сайту.</p>{{end}}
{{if .GraphPartial}}<p>Глибина попередня: відомі шляхи можуть бути не найкоротшими на повному сайті. Вхідні посилання не знайдені лише серед перевірених сторінок. Нуль знайдених редиректів не виключає редиректів серед неперевірених цілей.</p>{{end}}
{{if .Warning}}<p>{{.Warning}}</p>{{end}}</section>{{end}}
<section id="duplicate-content"><h2>Дублі та схожий контент</h2>
<p>Response HTML · модель {{.Duplicates.Model}}. Розглянуто сторінок: {{.Duplicates.Pages}}; проаналізовано: {{.Duplicates.Analyzed}}; неперевірені підписи: {{.Duplicates.Unverified}}; короткий контент: {{.Duplicates.ShortContent}}; поза місткістю аналізу: {{.Duplicates.CapacityExcluded}}.</p>
<p>Точних груп: {{.Duplicates.ExactClusters}}; схожих груп: {{.Duplicates.NearClusters}}; canonical для перевірки: {{.Duplicates.CanonicalReviews}}. Повнота пошуку схожих: {{.Duplicates.NearSearchComplete}}. Схожість не доводить канібалізацію; регіональні та товарні варіанти потребують контексту.</p>
{{range .Clusters}}<div><h3>Група {{.ID}} · {{.Kind}} · {{.Count}} сторінок</h3><p>{{.Context}} · canonical: {{.Canonical}} · потенційне перекриття для перевірки: {{.PotentialOverlap}}</p><p>Target ID: {{range .TargetIDs}}<a href="#target-{{.}}">{{.}}</a> {{end}}</p></div>{{end}}</section>
`))
