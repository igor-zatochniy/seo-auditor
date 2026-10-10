package geo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const OpportunityModel = "local-opportunity-v1"

type Confidence struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

func MatchingConfidence(r Result) Confidence {
	if r.TargetID == nil || r.Matching == nil {
		return Confidence{"unknown", "Відсутній достатній локальний збіг; це не доказ відсутності сторінки на сайті."}
	}
	if r.Ambiguous || r.Matching.Limited || len(r.Matching.Approximate) > 0 {
		return Confidence{"low", "Близькі кандидати, наближені збіги або неповна вибірка потребують ручної перевірки."}
	}
	if r.Matching.WeightedCoverage >= 80 && r.Coverage >= 80 {
		return Confidence{"high", "Високе покриття термінів без близьких кандидатів; не калібрована ймовірність правильності."}
	}
	return Confidence{"medium", "Частковий лексичний збіг; перевірте намір і повноту відповіді."}
}

type PageFacts struct {
	TargetID   int64  `json:"target_id"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Status     *int   `json:"http_status"`
	ScanStatus string `json:"scan_status"`
	ErrorCode  string `json:"error_code"`
	Inlinks    *int   `json:"inlinks"`
	Truncated  bool   `json:"links_truncated"`
}

type LinkPair struct{ From, To int64 }

type CompetitorProof struct {
	Ordinal  int       `json:"ordinal"`
	URL      string    `json:"url"`
	ReportID string    `json:"report_id"`
	Gap      SignalGap `json:"gap"`
}

type TaskEvidence struct {
	Source string `json:"source"`
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail"`
}

type Opportunity struct {
	ID         string         `json:"id"`
	Priority   string         `json:"priority"`
	Kind       string         `json:"kind"`
	TargetID   *int64         `json:"target_id"`
	TargetURL  string         `json:"target_url"`
	Clusters   []string       `json:"clusters"`
	Queries    []string       `json:"queries"`
	Action     string         `json:"action"`
	Confidence Confidence     `json:"confidence"`
	Evidence   []TaskEvidence `json:"evidence"`
}

type LinkSuggestion struct {
	From       int64      `json:"from_target_id"`
	To         int64      `json:"to_target_id"`
	SourceURL  string     `json:"source_url"`
	TargetURL  string     `json:"target_url"`
	Anchor     string     `json:"suggested_anchor"`
	Inlinks    int        `json:"observed_inlinks"`
	Relevance  int        `json:"relevance"`
	Confidence Confidence `json:"confidence"`
	Reason     string     `json:"reason"`
}

type PlanInput struct {
	Results     []Result
	Pages       []Page
	Facts       []PageFacts
	Edges       map[LinkPair]bool
	GraphReady  bool
	GraphFull   bool
	Competitors []CompetitorProof
}

type OpportunityPlan struct {
	Model    string           `json:"model"`
	Topics   []Topic          `json:"topics"`
	Tasks    []Opportunity    `json:"tasks"`
	Links    []LinkSuggestion `json:"links"`
	Warnings []string         `json:"warnings"`
	Limited  bool             `json:"limited"`
}

func BuildOpportunityPlan(ctx context.Context, input PlanInput) (OpportunityPlan, error) {
	p := OpportunityPlan{Model: OpportunityModel, Tasks: []Opportunity{}, Links: []LinkSuggestion{}, Warnings: []string{}}
	if len(input.Facts) > MaxPages || len(input.Pages) > MaxPages || len(input.Results) > MaxQueries || len(input.Edges) > MaxPages*256 || len(input.Competitors) > MaxQueries*5*32 {
		return p, fmt.Errorf("перевищено ліміт планування")
	}
	var err error
	p.Topics, err = ClusterTopics(ctx, input.Results)
	if err != nil {
		return p, err
	}
	cluster := map[int]string{}
	for _, t := range p.Topics {
		for _, q := range t.Queries {
			cluster[q.Ordinal] = t.Label
		}
	}
	byKey := map[string]int{}
	keys := []string{}
	add := func(key string, task Opportunity) {
		if i, exists := byKey[key]; exists {
			old := &p.Tasks[i]
			for _, q := range task.Queries {
				old.Queries = appendUnique(old.Queries, q, 12)
			}
			for _, c := range task.Clusters {
				old.Clusters = appendUnique(old.Clusters, c, 6)
			}
			for _, e := range task.Evidence {
				if len(old.Evidence) < 8 && !containsEvidence(old.Evidence, e) {
					old.Evidence = append(old.Evidence, e)
				}
			}
			return
		}
		if len(p.Tasks) >= 500 {
			p.Limited = true
			worst := 0
			for i := range p.Tasks {
				if priorityOrder(p.Tasks[i].Priority) > priorityOrder(p.Tasks[worst].Priority) {
					worst = i
				}
			}
			if priorityOrder(task.Priority) >= priorityOrder(p.Tasks[worst].Priority) {
				return
			}
			delete(byKey, keys[worst])
			hash := sha256.Sum256([]byte(key))
			task.ID = hex.EncodeToString(hash[:16])
			p.Tasks[worst] = task
			keys[worst] = key
			byKey[key] = worst
			return
		}
		hash := sha256.Sum256([]byte(key))
		task.ID = hex.EncodeToString(hash[:16])
		byKey[key] = len(p.Tasks)
		keys = append(keys, key)
		p.Tasks = append(p.Tasks, task)
	}
	for _, f := range input.Facts {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		if f.ScanStatus == "completed" && f.Status != nil && *f.Status >= 200 && *f.Status < 300 {
			continue
		}
		id := f.TargetID
		status := "HTTP не отримано"
		if f.Status != nil {
			status = fmt.Sprintf("HTTP %d", *f.Status)
		}
		add(fmt.Sprintf("access:%d", id), Opportunity{Priority: "high", Kind: "technical", TargetID: &id, TargetURL: f.URL, Clusters: []string{}, Queries: []string{}, Action: "Перевірити доступність і призначення сторінки перед змінами контенту.", Confidence: Confidence{"high", "Безпосередній результат збереженого HTTP-аудиту."}, Evidence: []TaskEvidence{{Source: "seo", URL: f.URL, Detail: status + " · " + f.ScanStatus + " · " + f.ErrorCode}}})
	}
	for i, r := range input.Results {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		conf := MatchingConfidence(r)
		base := Opportunity{TargetID: r.TargetID, TargetURL: r.TargetURL, Clusters: []string{cluster[i+1]}, Queries: []string{r.Query}, Confidence: conf}
		if r.TargetID == nil {
			t := base
			t.Priority, t.Kind, t.Action = "medium", "mapping", "Перевірити повноту обходу та обрати сторінку; створення нової сторінки вирішувати лише після ручної перевірки."
			t.Evidence = []TaskEvidence{{Source: "mapping", Detail: "У вибраному аудиті немає достатнього локального збігу для запиту: " + r.Query}}
			add("mapping:"+QueryKey(cluster[i+1]), t)
			continue
		}
		if r.Eligibility != nil {
			for _, c := range []struct {
				name  string
				value EligibilityCheck
			}{{"Google AI", r.Eligibility.GoogleAI}, {"ChatGPT Search", r.Eligibility.ChatGPTSearch}} {
				if c.value.Status != Blocked {
					continue
				}
				t := base
				t.Priority, t.Kind, t.Action = "high", "technical", "Перевірити доцільність обмеження "+c.name+"; не прибирати свідомо встановлені заборони автоматично."
				t.Confidence = Confidence{"high", "Виявлено явні технічні обмеження; дозвіл не гарантує цитування."}
				t.Evidence = []TaskEvidence{{Source: "seo", URL: r.TargetURL, Detail: strings.Join(c.value.Reasons, "; ")}}
				add(fmt.Sprintf("eligibility:%d:%s", *r.TargetID, c.name), t)
			}
		}
		for _, c := range r.Checks {
			if c.Passed || c.Recommendation == "" {
				continue
			}
			t := base
			t.Priority, t.Kind, t.Action = "medium", "content", c.Recommendation
			if c.Category == "schema" || c.Category == "metadata" {
				t.Priority = "low"
			}
			t.Evidence = []TaskEvidence{{Source: "content", URL: r.TargetURL, Detail: c.Name + ": " + c.Evidence}}
			add(fmt.Sprintf("check:%d:%s:%s", *r.TargetID, c.Category, c.Name), t)
		}
		if r.Ambiguous {
			t := base
			t.Priority, t.Kind, t.Action = "medium", "intent_review", "Порівняти близькі цілі та їхні наміри. Не видаляти й не об'єднувати сторінки лише на підставі лексичного збігу."
			t.Evidence = []TaskEvidence{{Source: "mapping", URL: r.TargetURL, Detail: "Є кандидати з різницею релевантності до 10 пунктів."}}
			add(fmt.Sprintf("intent:%d", *r.TargetID), t)
		}
		if r.Entities != nil {
			for _, c := range r.Entities.Checks {
				if c.Status != "missing" && c.Status != "mismatch" {
					continue
				}
				t := base
				t.Priority, t.Kind, t.Action = "low", "entity", "Перевірити узгодженість сигналу: "+c.Name+". Не додавати непідтверджені сутності чи посилання."
				status := "Не виявлено"
				if c.Status == "mismatch" {
					status = "Не узгоджено"
				}
				t.Evidence = []TaskEvidence{{Source: "entity", URL: r.TargetURL, Detail: c.Name + ": " + status}}
				add(fmt.Sprintf("entity:%d:%s", *r.TargetID, c.Key), t)
			}
		}
	}
	for _, c := range input.Competitors {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		if c.Ordinal < 1 || c.Ordinal > len(input.Results) {
			continue
		}
		r := input.Results[c.Ordinal-1]
		if r.TargetID == nil {
			continue
		}
		add(fmt.Sprintf("competitor:%d:%s", *r.TargetID, c.Gap.Signal), Opportunity{Priority: "medium", Kind: "competitor_gap", TargetID: r.TargetID, TargetURL: r.TargetURL, Clusters: []string{cluster[c.Ordinal]}, Queries: []string{r.Query}, Action: "Перевірити й за потреби посилити: " + c.Gap.Signal + ". Наявність у конкурента не доводить SEO-перевагу.", Confidence: Confidence{"medium", "Відмінність збережених сигналів двох аудитів; не доказ причин рейтингу."}, Evidence: []TaskEvidence{{Source: "competitor:" + c.ReportID, URL: c.URL, Detail: c.Gap.Evidence}}})
	}
	if !input.GraphReady {
		p.Warnings = append(p.Warnings, "Немає завершеного Site Crawl-графа: рекомендації перелінковки не обчислено.")
	} else {
		p.Links, err = recommendInternalLinks(ctx, input)
		if err != nil {
			return p, err
		}
		if !input.GraphFull {
			p.Warnings = append(p.Warnings, "Граф обмежений crawl/HTML-лімітами. Вхідні посилання — лише спостережені; рекомендації потребують перевірки на сайті.")
		}
		for _, l := range p.Links {
			id := l.To
			add(fmt.Sprintf("link:%d:%d", l.From, l.To), Opportunity{Priority: "medium", Kind: "internal_link", TargetID: &id, TargetURL: l.TargetURL, Clusters: []string{}, Queries: []string{l.Anchor}, Action: "Розглянути контекстне посилання з " + l.SourceURL + " з анкором «" + l.Anchor + "».", Confidence: l.Confidence, Evidence: []TaskEvidence{{Source: "graph", URL: l.SourceURL, Detail: l.Reason}}})
		}
	}
	sort.SliceStable(p.Tasks, func(i, j int) bool {
		a, b := priorityOrder(p.Tasks[i].Priority), priorityOrder(p.Tasks[j].Priority)
		if a != b {
			return a < b
		}
		return p.Tasks[i].ID < p.Tasks[j].ID
	})
	return p, nil
}

func appendUnique(items []string, value string, max int) []string {
	for _, s := range items {
		if s == value {
			return items
		}
	}
	if value != "" && len(items) < max {
		return append(items, value)
	}
	return items
}
func containsEvidence(items []TaskEvidence, e TaskEvidence) bool {
	for _, v := range items {
		if v == e {
			return true
		}
	}
	return false
}
func priorityOrder(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

func recommendInternalLinks(ctx context.Context, input PlanInput) ([]LinkSuggestion, error) {
	out := []LinkSuggestion{}
	corpus, err := buildLexicalCorpus(ctx, input.Pages)
	if err != nil {
		return out, err
	}
	facts := map[int64]PageFacts{}
	for _, f := range input.Facts {
		facts[f.TargetID] = f
	}
	seenTargets := map[int64]bool{}
	for _, r := range input.Results {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if r.TargetID == nil || seenTargets[*r.TargetID] {
			continue
		}
		id := *r.TargetID
		seenTargets[id] = true
		if !linkURLUsable(r.TargetURL) {
			continue
		}
		f, ok := facts[id]
		if !ok || f.Inlinks == nil || *f.Inlinks > 2 || r.Eligibility == nil || r.Eligibility.GoogleAI.Status == Blocked || r.Eligibility.ChatGPTSearch.Status == Blocked {
			continue
		}
		order, err := corpus.rank(ctx, r.Query, terms(r.Query))
		if err != nil {
			return out, err
		}
		added := 0
		for _, candidate := range order {
			page := input.Pages[candidate.page]
			if !linkURLUsable(page.URL) {
				continue
			}
			if page.TargetID == id || input.Edges[LinkPair{page.TargetID, id}] || candidate.coverage < 50 || candidate.matching.WeightedCoverage < 50 || candidate.matching.Limited || page.Signals == nil || !page.Signals.Complete || page.Signals.ContentIncomplete {
				continue
			}
			eligibility := EvaluateEligibility(page.Signals)
			if eligibility.GoogleAI.Status == Blocked || eligibility.ChatGPTSearch.Status == Blocked {
				continue
			}
			conf := Confidence{"medium", "Лексичний збіг і відсутність ребра в збереженому графі; потрібна редакторська перевірка контексту."}
			if !input.GraphFull || f.Truncated || facts[page.TargetID].Truncated || page.Signals.Version != 3 || page.Signals.SampleTruncated || len(candidate.matching.Approximate) > 0 {
				conf = Confidence{"low", "Обмежений граф або наближений збіг; спершу перевірте наявність посилання й контекст."}
			}
			out = append(out, LinkSuggestion{From: page.TargetID, To: id, SourceURL: page.URL, TargetURL: r.TargetURL, Anchor: r.Query, Inlinks: *f.Inlinks, Relevance: candidate.matching.Relevance, Confidence: conf, Reason: fmt.Sprintf("Спостережено %d вхідних посилань; тематичний збіг %d/100; збереженого ребра між цими Target ID немає. Поріг ≤2 є евристикою, не вимогою пошуковика.", *f.Inlinks, candidate.matching.Relevance)})
			added++
			if added >= 3 {
				break
			}
		}
		if len(out) >= 200 {
			return out[:200], nil
		}
	}
	return out, nil
}

// Ручний вибір не змінює автоматичну модель або її збережений результат.
func AnalyzeChosenTarget(ctx context.Context, query string, pages []Page, target int64) (Result, error) {
	if len(pages) > MaxPages {
		return Result{}, fmt.Errorf("перевищено ліміт сторінок")
	}
	corpus, err := buildLexicalCorpus(ctx, pages)
	if err != nil {
		return Result{}, err
	}
	qt := terms(query)
	order, err := corpus.rank(ctx, query, qt)
	if err != nil {
		return Result{}, err
	}
	for _, page := range pages {
		if page.TargetID != target {
			continue
		}
		id := target
		r := Result{Query: query, Intent: intent(query), TargetID: &id, TargetURL: page.URL, Level: "unknown", Matched: []string{}, Missing: qt, Alternatives: []Candidate{}, Checks: []Check{}, Gaps: []string{}, Warnings: []string{"Ціль обрано вручну; автоматичний результат збережено окремо."}}
		for _, c := range order {
			if pages[c.page].TargetID == target {
				r.Coverage, r.Matched, r.Missing, r.Matching = c.coverage, c.matched, c.missing, &c.matching
				break
			}
		}
		evaluate(&r, page, qt)
		return r, nil
	}
	return Result{}, fmt.Errorf("ціль не є успішною HTML-сторінкою цього GEO-аудиту")
}

func linkURLUsable(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.RawQuery == "" && (u.Scheme == "http" || u.Scheme == "https")
}
