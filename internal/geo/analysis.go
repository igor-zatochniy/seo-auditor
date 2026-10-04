package geo

import (
	"context"
	"errors"
	"math"
	"net"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxQueries = 1000
	MaxPages   = 1000
	Model      = "local-lexical-v1"
)

type Input struct {
	ID          string `json:"id"`
	SourceRunID string `json:"source_run_id"`
	Queries     string `json:"queries"`
	Domain      string `json:"domain"`
	Brand       string `json:"brand"`
}

func NormalizeDomain(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("вкажіть домен без шляху, порту та параметрів")
	}
	host, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")))
	if err != nil || net.ParseIP(host) != nil || len(host) > 253 || !strings.Contains(host, ".") {
		return "", errors.New("вкажіть коректний домен сайту")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("некоректний домен")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", errors.New("некоректний домен")
			}
		}
	}
	return host, nil
}

func InDomain(raw, domain string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")))
	return err == nil && (host == domain || strings.HasSuffix(host, "."+domain))
}

func Validate(input *Input) ([]string, error) {
	domain, err := NormalizeDomain(input.Domain)
	if err != nil {
		return nil, err
	}
	input.Domain, input.Brand = domain, strings.TrimSpace(input.Brand)
	if !cleanText(input.Brand, 120) || len(input.Queries) > 500000 {
		return nil, errors.New("бренд: до 120 символів; список запитів: до 500 КБ")
	}
	queries, seen := []string{}, map[string]bool{}
	for _, line := range strings.Split(input.Queries, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !cleanText(line, 300) {
			return nil, errors.New("запит має містити до 300 символів без керівних символів")
		}
		line = strings.Join(strings.Fields(line), " ")
		key := fold(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		queries = append(queries, line)
		if len(queries) > MaxQueries {
			return nil, errors.New("за один аналіз дозволено до 1000 унікальних запитів")
		}
	}
	if len(queries) == 0 {
		return nil, errors.New("додайте хоча б один запит")
	}
	return queries, nil
}

func cleanText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

func fold(s string) string { return cases.Fold().String(norm.NFKC.String(s)) }

var stopWords = strings.Fields("a an the and or for to of in on with by is are how what why best top vs alternatives як що чому для та і й або в у на з із це найкращий найкращі огляд альтернативи")

func terms(text string) []string {
	words := strings.FieldsFunc(fold(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen, out := map[string]bool{}, []string{}
	for _, word := range words {
		if utf8.RuneCountInString(word) < 2 || utf8.RuneCountInString(word) > 64 || seen[word] {
			continue
		}
		stop := false
		for _, s := range stopWords {
			if word == s {
				stop = true
				break
			}
		}
		if stop {
			continue
		}
		seen[word] = true
		out = append(out, word)
		if len(out) == 256 {
			break
		}
	}
	return out
}

func intent(query string) string {
	q := " " + fold(query) + " "
	for _, part := range []string{" best ", " top ", " vs ", "alternative", "comparison", "найкращ", "порівня", "альтернатив"} {
		if strings.Contains(q, part) {
			return "commercial"
		}
	}
	for _, part := range []string{" buy ", " pricing ", " price ", "купити", "ціна", "вартість"} {
		if strings.Contains(q, part) {
			return "transactional"
		}
	}
	for _, part := range []string{" how ", " what ", " why ", " guide ", "як ", "що ", "чому ", "інструкц"} {
		if strings.Contains(q, part) {
			return "informational"
		}
	}
	return "unknown"
}

type Page struct {
	TargetID                          int64
	URL, Title, Description, H1       string
	H1Count, ExternalLinks, WordCount int
	MetaRobots, XRobotsTag, Canonical string
	SelfCanonical                     bool
	Signals                           *Signals
}

type Candidate struct {
	TargetID int64  `json:"target_id"`
	URL      string `json:"url"`
	Coverage int    `json:"coverage"`
}

type Check struct {
	Name           string `json:"name"`
	Passed         bool   `json:"passed"`
	Evidence       string `json:"evidence"`
	Recommendation string `json:"recommendation"`
}

type Result struct {
	Query         string      `json:"query"`
	Intent        string      `json:"intent"`
	TargetID      *int64      `json:"target_id"`
	TargetURL     string      `json:"target_url"`
	Coverage      int         `json:"coverage"`
	Matched       []string    `json:"matched"`
	Missing       []string    `json:"missing"`
	Readiness     *int        `json:"readiness"`
	Level         string      `json:"level"`
	Ambiguous     bool        `json:"ambiguous"`
	SharedQueries int         `json:"shared_queries"`
	Alternatives  []Candidate `json:"alternatives"`
	Checks        []Check     `json:"checks"`
	Gaps          []string    `json:"gaps"`
	Warnings      []string    `json:"warnings"`
}

type posting struct {
	page   int
	weight int
}

func Analyze(ctx context.Context, queries []string, pages []Page) ([]Result, error) {
	if len(queries) > MaxQueries || len(pages) > MaxPages {
		return nil, errors.New("перевищено ліміт локального аналізу")
	}
	index := make(map[string][]posting)
	for i, page := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		weights := map[string]int{}
		add := func(text string, weight int) {
			for _, term := range terms(text) {
				weights[term] = max(weights[term], weight)
			}
		}
		add(page.Title, 4)
		add(page.H1, 4)
		add(page.Description, 2)
		if u, err := url.Parse(page.URL); err == nil {
			path, _ := url.PathUnescape(u.EscapedPath())
			add(path, 1)
		}
		if page.Signals != nil && page.Signals.Complete {
			add(page.Signals.Headings, 3)
			add(page.Signals.Excerpt, 1)
		}
		for term, weight := range weights {
			index[term] = append(index[term], posting{i, weight})
		}
	}
	results := make([]Result, 0, len(queries))
	counts := map[int64]int{}
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		qt := terms(query)
		matches, weights := make([]int, len(pages)), make([]int, len(pages))
		for _, term := range qt {
			for _, p := range index[term] {
				matches[p.page]++
				weights[p.page] += p.weight
			}
		}
		order := make([]int, len(pages))
		for i := range pages {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool {
			a, b := order[i], order[j]
			if matches[a] != matches[b] {
				return matches[a] > matches[b]
			}
			if weights[a] != weights[b] {
				return weights[a] > weights[b]
			}
			return pages[a].TargetID < pages[b].TargetID
		})
		r := Result{Query: query, Intent: intent(query), Level: "unknown", Matched: []string{}, Missing: []string{}, Alternatives: []Candidate{}, Checks: []Check{}, Gaps: []string{}, Warnings: []string{}}
		for _, i := range order {
			if matches[i] == 0 || len(r.Alternatives) == 3 {
				break
			}
			r.Alternatives = append(r.Alternatives, Candidate{pages[i].TargetID, pages[i].URL, int(math.Round(100 * float64(matches[i]) / float64(len(qt))))})
		}
		if len(r.Alternatives) == 0 || r.Alternatives[0].Coverage < 50 {
			r.Missing = qt
			r.Gaps = append(r.Gaps, "Відповідну сторінку не знайдено у вибраному аудиті. Перевірте повноту обходу та наявність цільової сторінки.")
		} else {
			best := order[0]
			page := pages[best]
			id := page.TargetID
			r.TargetID, r.TargetURL, r.Coverage = &id, page.URL, r.Alternatives[0].Coverage
			counts[id]++
			for _, term := range qt {
				found := false
				for _, p := range index[term] {
					if p.page == best {
						found = true
						break
					}
				}
				if found {
					r.Matched = append(r.Matched, term)
				} else {
					r.Missing = append(r.Missing, term)
				}
			}
			r.Ambiguous = len(r.Alternatives) > 1 && r.Alternatives[1].Coverage >= max(50, r.Coverage-10)
			if r.Ambiguous {
				r.Warnings = append(r.Warnings, "Є близькі кандидати. Це підстава для ручного порівняння, а не доказ канібалізації.")
			}
			if len(r.Missing) > 0 {
				r.Gaps = append(r.Gaps, "Перевірте покриття відсутніх термінів: "+strings.Join(r.Missing, ", "))
			}
			evaluate(&r, page, qt)
		}
		results = append(results, r)
	}
	for i := range results {
		if results[i].TargetID != nil {
			results[i].SharedQueries = counts[*results[i].TargetID]
		}
	}
	return results, nil
}

func evaluate(r *Result, page Page, qt []string) {
	if page.Signals == nil || !page.Signals.Complete || page.Signals.Version != 1 {
		r.Warnings = append(r.Warnings, "Немає нових сигналів контенту. Повторіть SEO-аудит для оцінки готовності.")
		return
	}
	s := page.Signals
	check := func(name string, passed bool, evidence, recommendation string) {
		r.Checks = append(r.Checks, Check{name, passed, evidence, recommendation})
		if !passed {
			r.Gaps = append(r.Gaps, recommendation)
		}
	}
	paragraph := terms(s.FirstParagraph)
	matched := 0
	for _, q := range qt {
		for _, term := range paragraph {
			if q == term {
				matched++
				break
			}
		}
	}
	check("Терміни запиту у першому абзаці", matched > 0 && matched*2 >= len(qt), s.FirstParagraph,
		"Перевірте, чи перший абзац прямо відповідає на запит; за потреби додайте стислу відповідь.")
	check("Головний заголовок", page.H1Count == 1, page.H1, "Перевірте наявність одного змістовного H1.")
	check("Структура відповіді", s.HasList || s.HasTable, boolEvidence(s.HasList, "Список")+"; "+boolEvidence(s.HasTable, "Таблиця"),
		"За доречності структуруйте відповідь списком або таблицею.")
	check("Авторство", s.HasAuthor, boolEvidence(s.HasAuthor, "Метадані або посилання автора"),
		"Перевірте видиме авторство та відповідальність за зміст; автоматичний сигнал не підтверджує експертність.")
	check("Зовнішні посилання", page.ExternalLinks > 0, boolEvidence(page.ExternalLinks > 0, "Зовнішні посилання"),
		"Для фактичних тверджень додайте релевантні першоджерела. Наявність посилання сама по собі не підтверджує достовірність.")
	if !s.SchemaIncomplete {
		check("Розпізнана структурована розмітка", len(s.SchemaTypes) > 0, strings.Join(s.SchemaTypes, ", "),
			"Перевірте доречну структуровану розмітку; тип має відповідати реальному вмісту сторінки.")
	} else {
		r.Warnings = append(r.Warnings, "JSON-LD не перевірено повністю: некоректний блок або перевищено ліміт розбору.")
	}
	if r.Intent == "commercial" {
		check("Табличний блок для порівняння", s.HasTable, boolEvidence(s.HasTable, "HTML-таблиця"),
			"Для порівняльного запиту перевірте наявність критеріїв, альтернатив і доказів. Таблиця доречна не завжди.")
	}
	if strings.Contains(fold(page.MetaRobots+" "+page.XRobotsTag), "noindex") || strings.Contains(fold(page.MetaRobots+" "+page.XRobotsTag), "none") {
		r.Warnings = append(r.Warnings, "Є обмежувальна robots-директива. Перевірте її scope та індексованість окремо.")
	}
	if page.Canonical != "" && !page.SelfCanonical {
		r.Warnings = append(r.Warnings, "Canonical вказує на іншу адресу; перевірте вибір цільової сторінки.")
	}
	if s.SampleTruncated {
		r.Warnings = append(r.Warnings, "Зіставлення використовує обмежену вибірку тексту, тому відсутній термін може бути далі на сторінці.")
	}
	passed := 0
	for _, c := range r.Checks {
		if c.Passed {
			passed++
		}
	}
	score := int(math.Round(100 * float64(passed) / float64(len(r.Checks))))
	r.Readiness = &score
	r.Level = "weak"
	if score >= 75 {
		r.Level = "strong"
	} else if score >= 45 {
		r.Level = "medium"
	}
}

func boolEvidence(v bool, name string) string {
	if v {
		return name + ": виявлено"
	}
	return name + ": не виявлено"
}
