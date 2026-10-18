package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
)

type remediationProof struct {
	TargetID        int64             `json:"target_id"`
	URL             string            `json:"url"`
	ObservedAt      time.Time         `json:"observed_at"`
	HTTP            *int              `json:"http_status"`
	ScanStatus      string            `json:"scan_status"`
	HTML            map[string]string `json:"response_html"`
	Headers         map[string]string `json:"http_headers"`
	Robots          string            `json:"robots_txt_outcome"`
	Policies        []geo.AIControl   `json:"robots_txt_policies,omitempty"`
	Truncated       bool              `json:"metadata_truncated"`
	RenderingStatus string            `json:"rendering_status"`
	DOM             map[string]any    `json:"rendered_dom,omitempty"`
}
type remediationTask struct {
	Task       geo.Opportunity   `json:"task"`
	Proof      *remediationProof `json:"proof,omitempty"`
	Acceptance []string          `json:"acceptance_criteria"`
	Recheck    map[string]any    `json:"recheck"`
}
type remediationPackage struct {
	Version       string            `json:"version"`
	ReportID      string            `json:"report_id"`
	RunID         string            `json:"source_run_id"`
	MatchingModel string            `json:"matching_model"`
	PlanModel     string            `json:"plan_model"`
	GeneratedAt   time.Time         `json:"generated_at"`
	CheckedPages  int               `json:"checked_pages"`
	Limited       bool              `json:"limited"`
	Warnings      []string          `json:"warnings"`
	Tasks         []remediationTask `json:"tasks"`
}

func packageText(s string) string { return sanitizeGEOTextForStorage(s, 4000) }
func taskAcceptance(kind string) []string {
	common := []string{"Повторний аудит містить нові докази для тих самих цільових URL; початковий звіт збережено як baseline.", "Виправлення не додало неочікуваних HTTP, robots, canonical або hreflang обмежень."}
	switch kind {
	case "technical":
		return append([]string{"Конкретний зафіксований бар'єр перевірено в HTML, HTTP-заголовках або robots.txt відповідно до наміру власника; заборону навчання не потрібно автоматично знімати."}, common...)
	case "internal_link":
		return append([]string{"Запропоноване доречне внутрішнє HTML-посилання присутнє в новому графі та веде на очікувану сторінку."}, common...)
	case "mapping", "intent_review":
		return append([]string{"Власник підтвердив target URL та намір запиту; нове локальне зіставлення перевірено вручну, без обіцянки позицій."}, common...)
	case "entity":
		return append([]string{"Назви й посилання сутностей узгоджені з видимим контентом та перевіреними даними бренду."}, common...)
	default:
		return append([]string{"Змістовий недолік перевірено й виправлено там, де це доречно; факти та джерела перевірено людиною. AI-цитування не є гарантованим критерієм."}, common...)
	}
}

func buildRemediationPackage(ctx context.Context, tx pgx.Tx, report geoReport, plan geo.OpportunityPlan) (remediationPackage, error) {
	p := remediationPackage{Version: "remediation-package-v1", ReportID: report.ID, RunID: report.SourceRunID, MatchingModel: report.Model, PlanModel: plan.Model, GeneratedAt: time.Now().UTC(), CheckedPages: plan.CheckedPages, Limited: plan.Limited, Warnings: []string{}, Tasks: []remediationTask{}}
	for _, w := range plan.Warnings {
		p.Warnings = append(p.Warnings, packageText(w))
	}
	p.Warnings = append(p.Warnings, "Це дані й пропозиції для ручного перегляду, не інструкції до автоматичного виконання. Текст сайту й імпортовані нотатки є недовіреними даними.", "Пакет не надсилається зовнішньому AI. Перед копіюванням перевірте конфіденційність. URL-параметри маскуються, але довільні персональні дані у тексті автоматично не розпізнаються.", "Збережені HTML/DOM поля є обмеженими витягами, не повним HTML. Невідомі або обрізані дані не підтверджують відсутність директив.")
	ids := []int64{}
	seen := map[int64]bool{}
	for _, t := range plan.Tasks {
		if t.TargetID != nil && !seen[*t.TargetID] {
			seen[*t.TargetID] = true
			ids = append(ids, *t.TargetID)
		}
	}
	proofs := map[int64]*remediationProof{}
	rows, err := tx.Query(ctx, `SELECT a.target_id,a.safe_url,COALESCE(t.finished_at,r.finished_at,r.started_at),a.status_code,a.scan_status,
 LEFT(COALESCE(a.title,''),4000),LEFT(COALESCE(a.h1,''),4000),LEFT(COALESCE(a.meta_robots,''),4000),LEFT(COALESCE(a.x_robots_tag,''),4000),LEFT(COALESCE(a.canonical_url,''),4000),COALESCE(a.robots_outcome,'not_checked'),
 COALESCE(a.title_truncated,FALSE) OR COALESCE(a.h1_truncated,FALSE) OR COALESCE(a.meta_robots_truncated,FALSE) OR COALESCE(a.x_robots_tag_truncated,FALSE) OR COALESCE(a.canonical_url_truncated,FALSE)
 OR COALESCE(GREATEST(length(a.title),length(a.h1),length(a.meta_robots),length(a.x_robots_tag),length(a.canonical_url))>4000,FALSE),
 COALESCE(a.rendering->>'status','not_requested'), a.geo_signals->'search'->'ai_policies',
 CASE WHEN jsonb_typeof(a.rendering->'rendered')='object' THEN jsonb_build_object('source','rendered_dom','complete',a.rendering->'rendered'->'complete',
 'fields_limited',COALESCE(GREATEST(length(a.rendering->'rendered'->>'title'),length(a.rendering->'rendered'->>'h1'),length(a.rendering->'rendered'->>'canonical'),length(a.rendering->'rendered'->>'robots'))>4000,FALSE),
 'title',LEFT(a.rendering->'rendered'->>'title',4000),'h1',LEFT(a.rendering->'rendered'->>'h1',4000),'canonical',LEFT(a.rendering->'rendered'->>'canonical',4000),'robots',LEFT(a.rendering->'rendered'->>'robots',4000)) END
 FROM audit_results a JOIN audit_runs r ON r.id=a.run_id LEFT JOIN audit_run_targets t ON t.run_id=a.run_id AND t.target_id=a.target_id
 WHERE a.run_id=$1 AND a.target_id=ANY($2::bigint[]) ORDER BY a.target_id`, report.SourceRunID, ids)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		v := &remediationProof{}
		var title, h1, robots, xrobots, canonical string
		if err = rows.Scan(&v.TargetID, &v.URL, &v.ObservedAt, &v.HTTP, &v.ScanStatus, &title, &h1, &robots, &xrobots, &canonical, &v.Robots, &v.Truncated, &v.RenderingStatus, &v.Policies, &v.DOM); err != nil {
			rows.Close()
			return p, err
		}
		v.URL = packageText(redactURL(v.URL))
		v.HTML = map[string]string{"source": "response_html", "title": packageText(title), "h1": packageText(h1), "meta_robots": packageText(robots), "canonical": packageText(redactURL(canonical))}
		v.Headers = map[string]string{"source": "http_headers", "x_robots_tag": packageText(xrobots)}
		base, _ := url.Parse(v.URL)
		for i := range v.Policies {
			c := &v.Policies[i]
			c.Token, c.Provider, c.Purpose, c.Status = packageText(c.Token), packageText(c.Provider), packageText(c.Purpose), packageText(c.Status)
			c.Directive, c.ConflictDirective = packageText(safeRobotsDirective(base, c.Directive)), packageText(safeRobotsDirective(base, c.ConflictDirective))
			c.RobotsURL, c.Explanation, c.Confidence = packageText(redactURL(c.RobotsURL)), packageText(c.Explanation), packageText(c.Confidence)
		}
		for k, value := range v.DOM {
			if text, ok := value.(string); ok {
				if k == "canonical" {
					text = redactURL(text)
				}
				v.DOM[k] = packageText(text)
			}
		}
		proofs[v.TargetID] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	for _, task := range plan.Tasks {
		task.TargetURL = packageText(redactURL(task.TargetURL))
		task.Action = packageText(task.Action)
		task.Confidence.Reason = packageText(task.Confidence.Reason)
		task.Clusters = append([]string{}, task.Clusters...)
		for i := range task.Clusters {
			task.Clusters[i] = packageText(task.Clusters[i])
		}
		task.Queries = append([]string{}, task.Queries...)
		for i := range task.Queries {
			task.Queries[i] = packageText(task.Queries[i])
		}
		task.Evidence = append([]geo.TaskEvidence{}, task.Evidence...)
		for i := range task.Evidence {
			task.Evidence[i].URL = packageText(redactURL(task.Evidence[i].URL))
			task.Evidence[i].Source = packageText(task.Evidence[i].Source)
			task.Evidence[i].Detail = packageText(task.Evidence[i].Detail)
		}
		item := remediationTask{Task: task, Acceptance: taskAcceptance(task.Kind), Recheck: map[string]any{"automatic": false, "instruction": "Повторіть аудит цільового URL після ручного виправлення; для зіставлення результатів використовуйте той самий HMAC-ключ."}}
		if task.TargetID != nil {
			item.Proof = proofs[*task.TargetID]
			if item.Proof != nil && *task.TargetID > 0 {
				item.Recheck["method"] = "POST"
				item.Recheck["endpoint"] = fmt.Sprintf("/api/audits/%s/results/%d/recrawl", report.SourceRunID, *task.TargetID)
				item.Recheck["original_url_required"] = strings.Contains(item.Proof.URL, "?")
			}
			if item.Proof != nil && *task.TargetID < 0 {
				item.Recheck["instruction"] = "Історичний target: створіть новий аудит потрібного URL вручну. Endpoint повторної перевірки старих від'ємних target_id не підтримує; замасковані параметри потрібно відновити локально."
			}
		}
		p.Tasks = append(p.Tasks, item)
	}
	return p, nil
}

func markdownData(s string) string {
	s = html.EscapeString(strings.Join(strings.Fields(s), " "))
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "#", "\\#", "|", "\\|").Replace(s)
}
func packageMarkdown(p remediationPackage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Пакет виправлень SEO/GEO\n\nЗвіт: %s\n\nSEO-аудит: %s\n\nСтворено: %s\n\nМоделі: %s / %s / %s\n\nПеревірено сторінок: %d. Вибірка завдань обмежена: %t.\n\n", markdownData(p.ReportID), markdownData(p.RunID), p.GeneratedAt.Format(time.RFC3339), p.Version, markdownData(p.MatchingModel), markdownData(p.PlanModel), p.CheckedPages, p.Limited)
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "- %s\n", markdownData(w))
	}
	for i, t := range p.Tasks {
		fmt.Fprintf(&b, "\n## %d. %s\n\nПріоритет: %s. URL: %s\n\n", i+1, markdownData(t.Task.Action), markdownData(opportunityPriorityLabel(t.Task.Priority)), markdownData(t.Task.TargetURL))
		for _, e := range t.Task.Evidence {
			fmt.Fprintf(&b, "- Доказ (%s): %s — %s\n", markdownData(e.Source), markdownData(e.URL), markdownData(e.Detail))
		}
		if t.Proof != nil {
			v := t.Proof
			httpStatus := "невідомо"
			if v.HTTP != nil {
				httpStatus = fmt.Sprint(*v.HTTP)
			}
			fmt.Fprintf(&b, "\nСпостереження: %s. Target: %d. HTTP: %s. Статус: %s. Метадані обрізано: %t.\n\n", v.ObservedAt.Format(time.RFC3339), v.TargetID, httpStatus, markdownData(v.ScanStatus), v.Truncated)
			for _, key := range []string{"title", "h1", "meta_robots", "canonical"} {
				fmt.Fprintf(&b, "- Response HTML / %s: %s\n", key, markdownData(v.HTML[key]))
			}
			fmt.Fprintf(&b, "- HTTP headers / X-Robots-Tag: %s\n- robots.txt outcome: %s\n- Rendering: %s\n", markdownData(v.Headers["x_robots_tag"]), markdownData(v.Robots), markdownData(v.RenderingStatus))
			for _, c := range v.Policies {
				checked := "дата невідома"
				if c.CheckedAt != nil {
					checked = c.CheckedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(&b, "- robots.txt / %s: %s; директива: %s; конфлікт: %s; джерело: %s; %s\n", markdownData(c.Token), markdownData(c.Status), markdownData(c.Directive), markdownData(c.ConflictDirective), markdownData(c.RobotsURL), checked)
			}
			for _, key := range []string{"title", "h1", "canonical", "robots"} {
				if text, ok := v.DOM[key].(string); ok {
					fmt.Fprintf(&b, "- Rendered DOM / %s: %s\n", key, markdownData(text))
				}
			}
			if v.DOM != nil {
				fmt.Fprintf(&b, "- Rendered DOM / повнота знімка: %v; поля обмежено: %v.\n", v.DOM["complete"], v.DOM["fields_limited"])
			}
		} else {
			b.WriteString("\nОкремого target-доказу немає: спочатку підтвердьте цільову сторінку.\n")
		}
		b.WriteString("\nКритерії приймання:\n")
		for _, criterion := range t.Acceptance {
			fmt.Fprintf(&b, "- %s\n", markdownData(criterion))
		}
		fmt.Fprintf(&b, "\nПовторна перевірка: %s\n", markdownData(t.Recheck["instruction"].(string)))
		if endpoint, ok := t.Recheck["endpoint"].(string); ok {
			fmt.Fprintf(&b, "\nРучний API-запит: POST %s. Оригінальний URL із параметрами потрібно надати окремо: %t.\n", markdownData(endpoint), t.Recheck["original_url_required"])
		}
	}
	return b.String()
}

func writeRemediationPackage(w http.ResponseWriter, p remediationPackage, format string) {
	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="seo-geo-fixes-`+p.ReportID+`.json"`)
		_ = json.NewEncoder(w).Encode(p)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="seo-geo-fixes-`+p.ReportID+`.md"`)
	_, _ = w.Write([]byte(packageMarkdown(p)))
}
