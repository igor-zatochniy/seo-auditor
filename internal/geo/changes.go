package geo

import "strconv"

type Change struct {
	Ordinal int    `json:"ordinal"`
	Query   string `json:"query"`
	Kind    string `json:"kind"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

// Порівнюємо лише перевірені сигнали. Unknown та неповна вибірка не означають втрату сигналу.
func CompareResults(previous, current []Result) []Change {
	old := map[string]Result{}
	for _, r := range previous {
		old[QueryKey(r.Query)] = r
	}
	changes := []Change{}
	for i, c := range current {
		p, ok := old[QueryKey(c.Query)]
		if !ok {
			continue
		}
		add := func(kind, before, after string) {
			if before != after {
				changes = append(changes, Change{i + 1, c.Query, kind, before, after})
			}
		}
		add("target_url", p.TargetURL, c.TargetURL)
		if p.Matching != nil && c.Matching != nil {
			add("relevance", strconv.Itoa(p.Matching.Relevance), strconv.Itoa(c.Matching.Relevance))
		}
		if p.Eligibility != nil && c.Eligibility != nil {
			for _, pair := range []struct{ kind, a, b string }{{"google_ai_eligibility", p.Eligibility.GoogleAI.Status, c.Eligibility.GoogleAI.Status}, {"chatgpt_search_eligibility", p.Eligibility.ChatGPTSearch.Status, c.Eligibility.ChatGPTSearch.Status}} {
				if knownEligibilityStatus(pair.a) && knownEligibilityStatus(pair.b) {
					add(pair.kind, pair.a, pair.b)
				}
			}
		}
		if p.Search != nil && c.Search != nil && (p.Search.OAISearchBotRules == Allowed || p.Search.OAISearchBotRules == Blocked) && (c.Search.OAISearchBotRules == Allowed || c.Search.OAISearchBotRules == Blocked) {
			add("oai_searchbot_rules", p.Search.OAISearchBotRules, c.Search.OAISearchBotRules)
		}
		for _, key := range []string{"visible_brand", "organization", "same_as", "publisher", "author", "product_brand"} {
			if a, b := entitySignalState(p.Entities, key), entitySignalState(c.Entities, key); a != "" && b != "" {
				kind := "entity:" + key
				if key == "visible_brand" {
					kind = "brand_entity"
				}
				add(kind, a, b)
			}
		}
		if p.Blocks != nil && c.Blocks != nil && p.Blocks.Complete && c.Blocks.Complete {
			add("answer_blocks", strconv.Itoa(relevantBlockCount(p)), strconv.Itoa(relevantBlockCount(c)))
		}
	}
	return changes
}

func knownEligibilityStatus(status string) bool {
	return status == Allowed || status == Blocked || status == Limited
}

func entitySignalState(e *EntityAssessment, key string) string {
	if e != nil {
		for _, check := range e.Checks {
			if check.Key == key && (check.Status == "found" || check.Status == "missing" || check.Status == "mismatch" || key == "product_brand" && check.Status == "not_applicable") {
				return check.Status
			}
		}
	}
	return ""
}
func relevantBlockCount(r Result) int {
	n := 0
	for _, block := range r.BlockMatches {
		if block.Coverage >= 50 {
			n++
		}
	}
	return n
}

type CitationObservation struct {
	Engine       string `json:"engine"`
	Citation     string `json:"citation"`
	BrandMention string `json:"brand_mention"`
}

func CompareCitations(ordinal int, query string, previous, current []CitationObservation) []Change {
	old := map[string]CitationObservation{}
	for _, c := range previous {
		old[c.Engine] = c
	}
	out := []Change{}
	for _, c := range current {
		p, ok := old[c.Engine]
		if !ok {
			continue
		}
		if (p.Citation == "yes" || p.Citation == "no") && (c.Citation == "yes" || c.Citation == "no") && p.Citation != c.Citation {
			out = append(out, Change{ordinal, query, "observed_citation:" + c.Engine, p.Citation, c.Citation})
		}
	}
	return out
}
