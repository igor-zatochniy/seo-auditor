package geo

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxVisibilityRows = 5000

type VisibilityInput struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Engine      string `json:"engine"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	Country     string `json:"country"`
	Device      string `json:"device"`
	CSV         string `json:"csv"`
}

type VisibilityRecord struct {
	Query              string   `json:"query"`
	URL                string   `json:"url"`
	Scope              string   `json:"scope"`
	Impressions        *int64   `json:"impressions"`
	Clicks             *int64   `json:"clicks"`
	CTR                *float64 `json:"ctr,omitempty"`
	Position           *float64 `json:"position,omitempty"`
	Sessions           *int64   `json:"sessions,omitempty"`
	SessionSource      string   `json:"session_source,omitempty"`
	AIReferral         bool     `json:"ai_referral,omitempty"`
	URLMatch           string   `json:"url_match,omitempty"`
	MatchedFingerprint string   `json:"matched_fingerprint,omitempty"`
	MatchKeyID         string   `json:"match_key_id,omitempty"`
	ReportOrdinal      *int     `json:"report_ordinal,omitempty"`
	TargetID           *int64   `json:"target_id,omitempty"`
	RequestURL         string   `json:"-"`
}

func QueryKey(query string) string { return normalizedBrand(query) }

// IsAIReferralSource is a conservative domain classifier for GA4 source values;
// it does not imply that an AI system cited or retrieved a page.
func IsAIReferralSource(raw string) bool {
	source := strings.TrimSpace(strings.ToLower(raw))
	if i := strings.Index(source, " / "); i >= 0 {
		source = strings.TrimSpace(source[:i])
	}
	if !strings.Contains(source, "://") {
		source = strings.SplitN(source, "/", 2)[0]
	}
	if parsed, err := url.Parse(source); err == nil && parsed.Host != "" {
		source = parsed.Hostname()
	} else if host, _, err := net.SplitHostPort(source); err == nil {
		source = host
	}
	source = strings.TrimSuffix(strings.Trim(source, "[] ."), ".")
	for _, domain := range []string{
		"chatgpt.com", "chat.openai.com", "perplexity.ai", "gemini.google.com",
		"bard.google.com", "copilot.microsoft.com", "claude.ai", "poe.com",
		"you.com", "phind.com",
	} {
		if source == domain || strings.HasSuffix(source, "."+domain) {
			return true
		}
	}
	return false
}

func ParseVisibilityCSV(input *VisibilityInput) ([]VisibilityRecord, error) {
	switch input.Source {
	case "gsc_web", "gsc_ai":
		input.Engine = "google_ai"
	case "ga4":
		input.Engine = "other"
	case "ai_observed":
		switch input.Engine {
		case "google_ai", "chatgpt", "gemini", "perplexity", "other":
		default:
			return nil, errors.New("оберіть AI-систему для імпортованих спостережень")
		}
	default:
		return nil, errors.New("невідоме джерело імпорту")
	}
	start, e1 := time.Parse("2006-01-02", input.PeriodStart)
	end, e2 := time.Parse("2006-01-02", input.PeriodEnd)
	if e1 != nil || e2 != nil || end.Before(start) || end.Sub(start) > 731*24*time.Hour {
		return nil, errors.New("вкажіть коректний період даних, не довший за два роки")
	}
	input.Country = strings.ToUpper(strings.TrimSpace(input.Country))
	if input.Country != "" && (len(input.Country) != 2 || input.Country[0] < 'A' || input.Country[0] > 'Z' || input.Country[1] < 'A' || input.Country[1] > 'Z') {
		return nil, errors.New("країна: дволітерний код, наприклад US або UA")
	}
	switch input.Device {
	case "", "all":
		input.Device = "all"
	case "desktop", "mobile", "tablet":
	default:
		return nil, errors.New("некоректний тип пристрою")
	}
	if !utf8.ValidString(input.CSV) || len(input.CSV) > 2<<20 || strings.ContainsRune(input.CSV, 0) {
		return nil, errors.New("CSV: UTF-8 без нульових символів, до 2 МіБ")
	}
	raw := strings.TrimPrefix(input.CSV, "\ufeff")
	reader := csv.NewReader(strings.NewReader(raw))
	if first, _, _ := strings.Cut(raw, "\n"); strings.Count(first, ";") > strings.Count(first, ",") {
		reader.Comma = ';'
	}
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil || len(header) > 8 {
		return nil, errors.New("некоректний заголовок CSV")
	}
	columns := map[string]int{}
	for i, name := range header {
		key := ""
		switch QueryKey(name) {
		case "query", "queries", "top queries", "запит", "запити", "найпопулярніші запити":
			key = "query"
		case "url", "page", "pages", "top pages", "сторінка", "сторінки", "найпопулярніші сторінки":
			key = "url"
		case "impressions", "покази":
			key = "impressions"
		case "clicks", "кліки":
			key = "clicks"
		case "ctr":
			key = "ctr"
		case "position", "average position", "позиція":
			key = "position"
		case "sessions", "сеанси":
			key = "sessions"
		case "session source", "session source / medium", "source", "source / medium", "джерело сеансу":
			key = "session_source"
		case "landing page", "landing page + query string", "page path", "сторінка входу":
			key = "url"
		default:
			return nil, fmt.Errorf("непідтримуваний стовпець %d; потрібні Query, URL/Page, Impressions та/або Clicks", i+1)
		}
		if _, exists := columns[key]; exists {
			return nil, errors.New("повторний стовпець CSV")
		}
		columns[key] = i
	}
	_, query := columns["query"]
	_, page := columns["url"]
	_, impressions := columns["impressions"]
	_, clicks := columns["clicks"]
	_, ctr := columns["ctr"]
	_, position := columns["position"]
	_, sessions := columns["sessions"]
	_, sessionSource := columns["session_source"]
	if input.Source != "ga4" && ((!query && !page) || (!impressions && !clicks && !ctr && !position)) {
		return nil, errors.New("CSV потребує запиту або URL та хоча б одного числового показника")
	}
	if input.Source == "gsc_ai" && (!page || !impressions) {
		return nil, errors.New("GSC generative AI потребує URL та Impressions; query і clicks не підтверджені цим звітом")
	}
	if input.Source == "gsc_ai" && (query || clicks || ctr || position || sessions || sessionSource) {
		return nil, errors.New("GSC generative AI: використовуйте лише URL та Impressions")
	}
	if input.Source == "gsc_web" && (sessions || sessionSource) {
		return nil, errors.New("GSC Web не приймає GA4 Sessions або Session source")
	}
	if input.Source == "ga4" && (!page || !sessions || !sessionSource || query || impressions || clicks || ctr || position) {
		return nil, errors.New("GA4 потребує Landing page, Session source і Sessions; інші метрики сюди не додавайте")
	}
	if input.Source == "ai_observed" && (ctr || position || sessions || sessionSource) {
		return nil, errors.New("AI-спостереження підтримують лише підтверджені покази та кліки")
	}
	value := func(row []string, key string) string {
		if i, ok := columns[key]; ok {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	metric := func(raw string) (*int64, error) {
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 || n > 1_000_000_000_000 {
			return nil, errors.New("числові показники мають бути невід'ємними цілими числами без розділювачів тисяч")
		}
		return &n, nil
	}
	decimal := func(raw string, percentage bool) (*float64, error) {
		if raw == "" {
			return nil, nil
		}
		raw = strings.TrimSpace(raw)
		isPercent := percentage && strings.HasSuffix(raw, "%")
		if isPercent {
			raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
		}
		if reader.Comma == ';' {
			raw = strings.ReplaceAll(raw, ",", ".")
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, errors.New("CTR і позиція мають бути скінченними невід'ємними числами")
		}
		if isPercent {
			n /= 100
		}
		if percentage && n > 1 || !percentage && n > 10000 {
			return nil, errors.New("CTR має бути в діапазоні 0–100%, позиція — від 0 до 10000")
		}
		return &n, nil
	}
	rows := []VisibilityRecord{}
	seen := map[string]bool{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("некоректний CSV у рядку %d", len(rows)+2)
		}
		r := VisibilityRecord{Query: value(row, "query"), RequestURL: value(row, "url"), SessionSource: value(row, "session_source")}
		if !cleanText(r.Query, 300) || len(r.RequestURL) > 2048 || (input.Source != "ga4" && r.Query == "" && r.RequestURL == "") {
			return nil, fmt.Errorf("некоректний запит або URL у рядку %d", len(rows)+2)
		}
		if input.Source == "ga4" && (!cleanText(r.SessionSource, 200) || r.RequestURL == "") {
			return nil, fmt.Errorf("некоректне джерело сеансу або Landing page у рядку %d", len(rows)+2)
		}
		r.Query = strings.Join(strings.Fields(r.Query), " ")
		r.SessionSource = strings.Join(strings.Fields(r.SessionSource), " ")
		if r.Impressions, err = metric(value(row, "impressions")); err != nil {
			return nil, err
		}
		if r.Clicks, err = metric(value(row, "clicks")); err != nil {
			return nil, err
		}
		if r.CTR, err = decimal(value(row, "ctr"), true); err != nil {
			return nil, err
		}
		if r.Position, err = decimal(value(row, "position"), false); err != nil {
			return nil, err
		}
		if r.Sessions, err = metric(value(row, "sessions")); err != nil {
			return nil, err
		}
		if input.Source == "ga4" {
			if r.Sessions == nil {
				return nil, errors.New("GA4: Sessions мають бути вказані для кожного рядка")
			}
			r.AIReferral = IsAIReferralSource(r.SessionSource)
		} else if r.Impressions == nil && r.Clicks == nil && r.CTR == nil && r.Position == nil {
			return nil, errors.New("рядок CSV не містить спостережених показників")
		}
		if r.Impressions != nil && r.Clicks != nil && *r.Clicks > *r.Impressions {
			return nil, errors.New("кліків не може бути більше за покази")
		}
		if input.Source == "gsc_ai" && (r.Query != "" || r.Clicks != nil || r.CTR != nil || r.Position != nil) {
			return nil, errors.New("GSC generative AI: query та clicks мають бути порожніми; не переносіть їх зі звичайного Web-звіту")
		}
		r.Scope = "query_url"
		if input.Source == "ga4" {
			r.Scope = "url"
		} else {
			if r.Query == "" {
				r.Scope = "url"
			}
			if r.RequestURL == "" {
				r.Scope = "query"
			}
		}
		key := QueryKey(r.Query) + "\x00" + r.RequestURL
		if input.Source == "ga4" {
			key += "\x00" + strings.ToLower(r.SessionSource)
		}
		if seen[key] {
			return nil, errors.New("CSV містить повторний запит/URL; об'єднайте рядки одного періоду перед імпортом")
		}
		seen[key] = true
		rows = append(rows, r)
		if len(rows) > MaxVisibilityRows {
			return nil, errors.New("один імпорт обмежено 5000 рядками")
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV не містить даних")
	}
	return rows, nil
}
