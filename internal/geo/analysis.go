package geo

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/performance"
	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxQueries = 1000
	MaxPages   = 1000
	Model      = "local-lexical-v4"
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
	TargetID  int64  `json:"target_id"`
	URL       string `json:"url"`
	Coverage  int    `json:"coverage"`
	Relevance int    `json:"relevance,omitempty"`
}

type Check struct {
	Name           string `json:"name"`
	Passed         bool   `json:"passed"`
	Evidence       string `json:"evidence"`
	Recommendation string `json:"recommendation"`
	Category       string `json:"category,omitempty"`
}

type Category struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Passed int    `json:"passed"`
	Total  int    `json:"total"`
	Level  string `json:"level"`
}

type Result struct {
	Query         string              `json:"query"`
	Intent        string              `json:"intent"`
	TargetID      *int64              `json:"target_id"`
	TargetURL     string              `json:"target_url"`
	Coverage      int                 `json:"coverage"`
	Matched       []string            `json:"matched"`
	Missing       []string            `json:"missing"`
	Readiness     *int                `json:"readiness"`
	Level         string              `json:"level"`
	Ambiguous     bool                `json:"ambiguous"`
	SharedQueries int                 `json:"shared_queries"`
	Alternatives  []Candidate         `json:"alternatives"`
	Checks        []Check             `json:"checks"`
	Gaps          []string            `json:"gaps"`
	Warnings      []string            `json:"warnings"`
	Categories    []Category          `json:"categories,omitempty"`
	Search        *SearchControls     `json:"search,omitempty"`
	ContentSource string              `json:"content_source,omitempty"`
	HTTP          *HTTPObservation    `json:"http,omitempty"`
	Performance   *performance.Result `json:"performance,omitempty"`
	Blocks        *BlockSample        `json:"blocks,omitempty"`
	BlockMatches  []BlockMatch        `json:"block_matches,omitempty"`
	Matching      *Matching           `json:"matching,omitempty"`
	Eligibility   *AIEligibility      `json:"eligibility,omitempty"`
	Entities      *EntityAssessment   `json:"entities,omitempty"`
}

type posting struct {
	page   int
	weight int
}

func Analyze(ctx context.Context, queries []string, pages []Page) ([]Result, error) {
	if len(queries) > MaxQueries || len(pages) > MaxPages {
		return nil, errors.New("перевищено ліміт локального аналізу")
	}
	corpus, err := buildLexicalCorpus(ctx, pages)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(queries))
	counts := map[int64]int{}
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		qt := terms(query)
		order, err := corpus.rank(ctx, query, qt)
		if err != nil {
			return nil, err
		}
		r := Result{Query: query, Intent: intent(query), Level: "unknown", Matched: []string{}, Missing: []string{}, Alternatives: []Candidate{}, Checks: []Check{}, Gaps: []string{}, Warnings: []string{}}
		for _, candidate := range order {
			if len(r.Alternatives) == 3 {
				break
			}
			page := pages[candidate.page]
			r.Alternatives = append(r.Alternatives, Candidate{TargetID: page.TargetID, URL: page.URL, Coverage: candidate.coverage, Relevance: candidate.matching.Relevance})
		}
		if len(order) == 0 || order[0].coverage < 50 || order[0].matching.WeightedCoverage < 50 {
			r.Missing = qt
			r.Gaps = append(r.Gaps, "Відповідну сторінку не знайдено у вибраному аудиті. Перевірте повноту обходу та наявність цільової сторінки.")
		} else {
			best := order[0]
			page := pages[best.page]
			id := page.TargetID
			r.TargetID, r.TargetURL, r.Coverage = &id, page.URL, r.Alternatives[0].Coverage
			counts[id]++
			r.Matched, r.Missing, r.Matching = best.matched, best.missing, &best.matching
			r.Ambiguous = len(order) > 1 && order[1].coverage >= 50 && order[1].matching.WeightedCoverage >= 50 && order[1].matching.Relevance >= best.matching.Relevance-10
			if len(best.matching.Approximate) > 0 {
				r.Warnings = append(r.Warnings, "Є збіги словоформ або друкарських помилок; перевірте запропоновану сторінку вручну.")
			}
			if best.matching.Limited {
				r.Warnings = append(r.Warnings, "Застосовано ліміти локального індексу або наближеного пошуку; деякі терміни чи фрази могли не потрапити до зіставлення.")
			}
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
	if page.Signals == nil || !page.Signals.Complete || page.Signals.Version < 1 || page.Signals.Version > 3 {
		r.Warnings = append(r.Warnings, "Немає нових сигналів контенту. Повторіть SEO-аудит для оцінки готовності.")
		return
	}
	s := page.Signals
	r.Eligibility = EvaluateEligibility(s)
	if r.Eligibility.GoogleAI.Status == Blocked || r.Eligibility.ChatGPTSearch.Status == Blocked {
		r.Warnings = append(r.Warnings, "Виявлено технічні обмеження AI-пошуку. Перевірте окремі блоки Google AI / ChatGPT Search; оцінка структури контенту їх не скасовує.")
	}
	r.HTTP, r.Performance, r.Blocks = s.HTTP, s.Performance, s.Blocks
	r.BlockMatches = matchBlocks(s.Blocks, qt)
	r.Search = &SearchControls{GooglebotRules: Unknown, OAISearchBotRules: Unknown, GoogleIndexing: Unknown, GoogleSnippet: Unknown}
	if s.Version >= 2 {
		copy := s.Search
		r.Search = &copy
		r.ContentSource = s.ContentSource
	} else {
		r.Warnings = append(r.Warnings, "Сигнали попередньої версії: вибірка могла містити меню; правила пошукових ботів не перевірені. Для нової методики повторіть SEO-аудит.")
	}
	if s.ContentIncomplete {
		r.Warnings = append(r.Warnings, "Перевищено ліміт структури HTML для GEO. Категорії контенту не оцінено; звичайні SEO-метрики збережені.")
		return
	}
	if s.Version == 2 {
		r.Warnings = append(r.Warnings, "Вибірка контенту попередньої версії могла містити форми чи службові header/footer. Для уточнення повторіть SEO-аудит.")
	}
	check := func(category, name string, passed bool, evidence, recommendation string) {
		r.Checks = append(r.Checks, Check{Name: name, Passed: passed, Evidence: evidence, Recommendation: recommendation, Category: category})
		if !passed && category != "metadata" {
			r.Gaps = append(r.Gaps, recommendation)
		}
	}
	paragraph := terms(s.FirstParagraph)
	matched := 0
	for _, q := range qt {
		for _, term := range paragraph {
			if q == term || wordFamily(q) == wordFamily(term) {
				matched++
				break
			}
		}
	}
	check("answer", "Терміни запиту у першому абзаці", matched > 0 && matched*2 >= len(qt), s.FirstParagraph,
		"Перевірте, чи перший абзац прямо відповідає на запит; за потреби додайте стислу відповідь.")
	check("answer", "Головний заголовок документа", page.H1Count == 1, page.H1, "Перевірте наявність одного змістовного H1.")
	check("answer", "Структура відповіді", s.HasList || s.HasTable, boolEvidence(s.HasList, "Список")+"; "+boolEvidence(s.HasTable, "Таблиця"),
		"За доречності структуруйте відповідь списком або таблицею.")
	check("evidence", "Авторство", s.HasAuthor, boolEvidence(s.HasAuthor, "Метадані або посилання автора"),
		"Перевірте видиме авторство та відповідальність за зміст; автоматичний сигнал не підтверджує експертність.")
	check("evidence", "Зовнішні посилання документа", page.ExternalLinks > 0, boolEvidence(page.ExternalLinks > 0, "Зовнішні посилання"),
		"Для фактичних тверджень додайте релевантні першоджерела. Наявність посилання сама по собі не підтверджує достовірність.")
	if !s.SchemaIncomplete {
		check("metadata", "Розпізнана структурована розмітка", len(s.SchemaTypes) > 0, strings.Join(s.SchemaTypes, ", "),
			"Перевірте доречну структуровану розмітку; тип має відповідати реальному вмісту сторінки.")
	} else {
		r.Warnings = append(r.Warnings, "JSON-LD не перевірено повністю: некоректний блок або перевищено ліміт розбору.")
	}
	if r.Intent == "commercial" {
		check("answer", "Табличний блок для порівняння", s.HasTable, boolEvidence(s.HasTable, "HTML-таблиця"),
			"Для порівняльного запиту перевірте наявність критеріїв, альтернатив і доказів. Таблиця доречна не завжди.")
	}
	if r.Search.GoogleIndexing == Blocked || r.Search.GoogleSnippet == Blocked {
		r.Warnings = append(r.Warnings, "Є обмеження індексації або текстового snippet для Google. Дозвіл robots.txt не скасовує цих директив.")
	}
	if r.Search.MaxSnippet != nil && *r.Search.MaxSnippet > 0 {
		r.Warnings = append(r.Warnings, "max-snippet обмежує обсяг текстового фрагмента; це не гарантує і не виключає AI-цитування.")
	}
	if r.Search.DataNoSnippet {
		r.Warnings = append(r.Warnings, "Виявлено data-nosnippet: окремі фрагменти виключені зі snippet, але це не заборона всієї сторінки.")
	}
	if page.Canonical != "" && !page.SelfCanonical {
		r.Warnings = append(r.Warnings, "Canonical вказує на іншу адресу; перевірте вибір цільової сторінки.")
	}
	if s.SampleTruncated {
		r.Warnings = append(r.Warnings, "Зіставлення використовує обмежену вибірку тексту, тому відсутній термін може бути далі на сторінці.")
	}
	r.Categories = []Category{{Key: "answer", Name: "Структура відповіді"}, {Key: "evidence", Name: "Авторство та посилання"}, {Key: "metadata", Name: "Розмітка (необов'язкова)"}}
	r.Level = "strong"
	for i := range r.Categories {
		c := &r.Categories[i]
		for _, check := range r.Checks {
			if check.Category == c.Key {
				c.Total++
				if check.Passed {
					c.Passed++
				}
			}
		}
		c.Level = "unknown"
		if c.Total > 0 {
			c.Level = "weak"
			if c.Passed == c.Total {
				c.Level = "strong"
			} else if c.Passed > 0 {
				c.Level = "medium"
			}
		}
		// Необов'язкова schema не знижує оцінку контенту.
		if c.Key != "metadata" && c.Level != "strong" {
			r.Level = "medium"
		}
	}
	if r.Categories[0].Passed == 0 {
		r.Level = "weak"
	}
}

func boolEvidence(v bool, name string) string {
	if v {
		return name + ": виявлено"
	}
	return name + ": не виявлено"
}
