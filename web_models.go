package main

import "time"

type webRun struct {
	RenderJavaScript bool       `json:"render_javascript"`
	ID               string     `json:"id"`
	Mode             string     `json:"mode"`
	Status           string     `json:"status"`
	Total            int64      `json:"total"`
	Successful       int64      `json:"successful"`
	Failed           int64      `json:"failed"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	Resumable        bool       `json:"resumable"`
}

type webProgress struct {
	Run        webRun           `json:"run"`
	Counts     map[string]int64 `json:"counts"`
	Percentage float64          `json:"percentage"`
}

type reportField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// This allowlist is shared by the API, HTML and CSV; private storage fields are absent.
var reportFields = []reportField{
	{"target_id", "Target ID", "Огляд"}, {"safe_url", "URL", "Огляд"},
	{"status_code", "HTTP", "Огляд"}, {"scan_status", "Статус", "Огляд"},
	{"is_redirect", "Редирект", "Огляд"}, {"redirect_url", "Адреса редиректу", "Огляд"},
	{"title", "Title", "Метадані"}, {"title_char_count", "Title: символів", "Метадані"},
	{"title_width_px", "Title: ширина, px", "Метадані"}, {"title_status", "Title: SERP width", "Метадані"},
	{"description", "Description", "Метадані"}, {"description_char_count", "Description: символів", "Метадані"},
	{"description_width_px", "Description: ширина, px", "Метадані"},
	{"description_status", "Description: Desktop", "Метадані"}, {"description_mobile_status", "Description: Mobile", "Метадані"},
	{"serp_width_model", "Модель оцінки ширини", "Метадані"}, {"serp_width_approximate", "Fallback / складне письмо", "Метадані"},
	{"h1", "H1", "Заголовки"}, {"h1_count", "Кількість H1", "Заголовки"}, {"h2_to_h6_status", "H2–H6", "Заголовки"},
	{"canonical_url", "Canonical", "Canonical"}, {"is_self_canonical", "Self-canonical", "Canonical"},
	{"meta_robots", "Meta robots", "Robots"}, {"x_robots_tag", "X-Robots-Tag", "Robots"},
	{"robots_allowed", "Доступ robots.txt", "Robots"}, {"robots_outcome", "Результат robots.txt", "Robots"},
	{"og_title", "OG title", "Соціальні метадані"}, {"og_description", "OG description", "Соціальні метадані"},
	{"og_image", "OG image", "Соціальні метадані"}, {"twitter_card", "Twitter card", "Соціальні метадані"},
	{"internal_links_count", "Внутрішні посилання", "Посилання"}, {"external_links_count", "Зовнішні посилання", "Посилання"},
	{"links_count", "Усі посилання", "Посилання"}, {"total_images", "Зображення", "Зображення"},
	{"images_missing_alt", "Без атрибута alt", "Зображення"}, {"has_json_ld", "JSON-LD", "Технічні сигнали"},
	{"has_viewport", "Viewport", "Технічні сигнали"}, {"word_count", "Слова", "Контент"},
	{"html_raw_bytes", "HTML після розпакування, bytes", "Розмір HTML"},
	{"html_size_complete", "HTML прочитано повністю", "Розмір HTML"},
	{"googlebot_2mb_status", "Googlebot: 2 MiB", "Розмір HTML"},
	{"rendering_status", "JavaScript: статус", "JavaScript"},
	{"rendering", "HTML відповіді / DOM після JavaScript", "JavaScript"},
	{"geo_signals", "Доступ ботів і фрагменти контенту", "GEO-діагностика"},
	{"crawl_depth", "Кліків від стартової сторінки", "Граф сайту"},
	{"in_sitemap", "Є в sitemap", "Граф сайту"},
	{"internal_inlinks_count", "Вхідні внутрішні сторінки", "Граф сайту"},
	{"internal_outlinks_count", "Унікальні внутрішні URL", "Граф сайту"},
	{"broken_internal_links", "Внутрішні URL з HTTP 4xx/5xx", "Граф сайту"},
	{"redirecting_internal_links", "Внутрішні URL з редиректом", "Граф сайту"},
	{"orphan_candidate", "Orphan candidate", "Граф сайту"},
	{"links_truncated", "Ліміт збору посилань", "Граф сайту"},
	{"site_graph_ready", "Метрики графа завершено", "Граф сайту"},
	{"duration_ms", "Час, ms", "Час"}, {"created_at", "Збережено", "Час"},
	{"attempts", "Спроби", "Час"}, {"started_at", "Початок target", "Час"}, {"finished_at", "Завершення target", "Час"},
	{"error_code", "Код помилки", "Помилки"}, {"error_message", "Помилка", "Помилки"},
}

var truncationFields = []string{"safe_url", "redirect_url", "title", "h1", "og_title", "og_image", "twitter_card", "canonical_url", "meta_robots", "x_robots_tag"}

func init() {
	for _, name := range truncationFields {
		reportFields = append(reportFields, reportField{name + "_truncated", name + ": обрізано", "Зберігання"},
			reportField{name + "_original_length", name + ": початкова довжина", "Зберігання"})
	}
}

type reportRecord map[string]any

type analyticsBucket struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}
type analyticsGroup struct {
	Label   string            `json:"label"`
	Buckets []analyticsBucket `json:"buckets"`
	Maximum float64           `json:"-"`
}
type webAnalytics struct {
	Total  int64            `json:"total"`
	Parsed int64            `json:"parsed"`
	Groups []analyticsGroup `json:"groups"`
}
