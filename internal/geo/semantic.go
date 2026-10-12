package geo

import (
	"context"
	"math"

	"github.com/igor-zatochniy/seo-auditor/internal/semantic"
)

const SemanticModel = Model + "+" + semantic.Model
const SemanticTopicModel = TopicModel + "+" + semantic.Model

type LexicalSelection struct {
	TargetID     *int64      `json:"target_id"`
	TargetURL    string      `json:"target_url"`
	Coverage     int         `json:"coverage"`
	Matching     *Matching   `json:"matching,omitempty"`
	Alternatives []Candidate `json:"alternatives"`
}

type SemanticAssessment struct {
	Model      string          `json:"model"`
	Selected   bool            `json:"selected"`
	Limited    bool            `json:"limited"`
	Concepts   []semantic.Unit `json:"concepts"`
	Candidates []Candidate     `json:"candidates"`
	Matching   *Matching       `json:"matching,omitempty"`
	Reason     string          `json:"reason"`
}

func PrepareSemanticQuery(ctx context.Context, query string, count int, dictionary LexicalDictionary) (IndexedQuery, []semantic.Unit, error) {
	units, limited := semantic.Extract(query)
	q := IndexedQuery{CorpusPages: count, Limited: limited, Variants: []QueryVariant{}}
	for i, u := range units {
		if err := ctx.Err(); err != nil {
			return q, units, err
		}
		q.Terms = append(q.Terms, u.Label)
		variants := []QueryVariant{}
		seen := map[string]bool{}
		for _, alias := range semantic.SingleTerms(u) {
			forms, err := dictionary.Forms(ctx, wordFamily(alias))
			if err != nil {
				return q, units, err
			}
			if len(forms) > 16 {
				forms = forms[:16]
				q.Limited = true
			}
			for _, term := range append([]string{alias}, forms...) {
				if seen[term] {
					continue
				}
				if len(q.Variants)+len(variants) >= 512 {
					q.Limited = true
					continue
				}
				seen[term] = true
				quality, kind := .82, "semantic"
				if term == u.Observed {
					quality, kind = 1, "exact"
				} else if !u.Known {
					quality, kind = .85, "morphology"
				}
				variants = append(variants, QueryVariant{Index: i, Term: term, Kind: kind, Quality: quality})
			}
		}
		keys := make([]string, len(variants))
		for j, v := range variants {
			keys[j] = v.Term
		}
		df, err := dictionary.Frequency(ctx, keys)
		if err != nil {
			return q, units, err
		}
		idf := 1 + math.Log(1+(float64(count-df)+.5)/(float64(df)+.5))
		q.TotalIDF += idf
		for j := range variants {
			variants[j].IDF = idf
		}
		q.Variants = append(q.Variants, variants...)
	}
	return q, units, ctx.Err()
}

func CombineSemantic(query string, lexical, semanticCandidates []RankedCandidate, units []semantic.Unit, limited bool) Result {
	original := ResultFromCandidates(query, lexical)
	alternative := ResultFromCandidates(query, semanticCandidates)
	assessment := &SemanticAssessment{Model: semantic.Model, Limited: limited, Concepts: units, Candidates: alternative.Alternatives, Matching: alternative.Matching,
		Reason: "Збережено лексичну ціль: словникова евристика не дала достатньо переконливого поліпшення."}
	known := 0
	for _, u := range units {
		if u.Known {
			known++
		}
	}
	selected := original
	oldRelevance := 0
	if original.Matching != nil {
		oldRelevance = original.Matching.Relevance
	}
	if known > 0 && len(units) >= 2 && !limited && alternative.TargetID != nil && !alternative.Ambiguous && alternative.Matching != nil &&
		!alternative.Matching.Limited && alternative.Matching.WeightedCoverage >= 65 && (original.TargetID == nil || alternative.Matching.Relevance >= oldRelevance+8) {
		selected = alternative
		assessment.Selected = true
		assessment.Reason = "Ціль обрано за міжмовними або синонімічними поняттями, IDF повного корпусу та відсутністю близького конкурента. Перевірте відповідність наміру вручну."
	}
	if known == 0 {
		assessment.Reason = "У контрольованому словнику не знайдено понять; використано початковий лексичний результат."
	}
	if limited {
		assessment.Reason = "Спрацював ліміт словникового зіставлення; автоматичну лексичну ціль не замінено."
	}
	selected.Lexical = &LexicalSelection{original.TargetID, original.TargetURL, original.Coverage, original.Matching, original.Alternatives}
	selected.Semantic = assessment
	return selected
}

func TopicTerms(r Result) []string {
	if r.Semantic == nil {
		return terms(r.Query)
	}
	units, _ := semantic.Extract(r.Query)
	out := make([]string, 0, len(units))
	for _, u := range units {
		if u.Known {
			out = append(out, "concept_"+u.Key)
		} else {
			out = append(out, u.Key)
		}
	}
	return out
}
