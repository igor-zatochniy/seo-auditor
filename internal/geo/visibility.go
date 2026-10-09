package geo

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
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
	Query         string `json:"query"`
	URL           string `json:"url"`
	Scope         string `json:"scope"`
	Impressions   *int64 `json:"impressions"`
	Clicks        *int64 `json:"clicks"`
	ReportOrdinal *int   `json:"report_ordinal,omitempty"`
	TargetID      *int64 `json:"target_id,omitempty"`
	RequestURL    string `json:"-"`
}

func QueryKey(query string) string { return normalizedBrand(query) }

func ParseVisibilityCSV(input *VisibilityInput) ([]VisibilityRecord, error) {
	switch input.Source {
	case "gsc_web", "gsc_ai":
		input.Engine = "google_ai"
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
		case "ctr", "position", "позиція":
			continue
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
	if (!query && !page) || (!impressions && !clicks) {
		return nil, errors.New("CSV потребує запиту або URL та хоча б одного числового показника")
	}
	if input.Source == "gsc_ai" && (!page || !impressions) {
		return nil, errors.New("GSC generative AI потребує URL та Impressions; query і clicks не підтверджені цим звітом")
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
			return nil, errors.New("покази й кліки мають бути невід'ємними цілими числами без розділювачів тисяч")
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
		r := VisibilityRecord{Query: value(row, "query"), RequestURL: value(row, "url")}
		if !cleanText(r.Query, 300) || len(r.RequestURL) > 2048 || (r.Query == "" && r.RequestURL == "") {
			return nil, fmt.Errorf("некоректний запит або URL у рядку %d", len(rows)+2)
		}
		r.Query = strings.Join(strings.Fields(r.Query), " ")
		if r.Impressions, err = metric(value(row, "impressions")); err != nil {
			return nil, err
		}
		if r.Clicks, err = metric(value(row, "clicks")); err != nil {
			return nil, err
		}
		if r.Impressions == nil && r.Clicks == nil {
			return nil, errors.New("рядок CSV не містить спостережених показників")
		}
		if r.Impressions != nil && r.Clicks != nil && *r.Clicks > *r.Impressions {
			return nil, errors.New("кліків не може бути більше за покази")
		}
		if input.Source == "gsc_ai" && (r.Query != "" || r.Clicks != nil) {
			return nil, errors.New("GSC generative AI: query та clicks мають бути порожніми; не переносіть їх зі звичайного Web-звіту")
		}
		r.Scope = "query_url"
		if r.Query == "" {
			r.Scope = "url"
		}
		if r.RequestURL == "" {
			r.Scope = "query"
		}
		key := QueryKey(r.Query) + "\x00" + r.RequestURL
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
