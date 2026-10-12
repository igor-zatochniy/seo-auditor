package geo

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Профіль однієї сторінки; ті самі межі вибірки, що й у local-lexical-v4.
type LexicalProfile struct {
	Terms   []LexicalTerm
	Phrases []string
	Limited bool
}

type LexicalTerm struct {
	Term, Family, First string
	Length, Weight      int
	Fuzzy               bool
}

func BuildLexicalProfile(ctx context.Context, page Page) (LexicalProfile, error) {
	p := LexicalProfile{Terms: []LexicalTerm{}, Phrases: []string{}}
	weights, phrases := map[string]int{}, map[string]bool{}
	add := func(text string, weight int) {
		words := sequence(text)
		if len(words) == maxPageTerms {
			p.Limited = true
		}
		for _, term := range words {
			if _, exists := weights[term]; !exists && len(weights) == maxPageTerms {
				p.Limited = true
				continue
			}
			weights[term] = max(weights[term], weight)
		}
		for n := 2; n <= 3; n++ {
			for j := 0; j+n <= len(words); j++ {
				key := phraseKey(words[j : j+n])
				if len(phrases) < maxPagePhrases {
					phrases[key] = true
				} else if !phrases[key] {
					p.Limited = true
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	add(page.Title, 4)
	add(page.H1, 4)
	complete := page.Signals != nil && page.Signals.Complete && !page.Signals.ContentIncomplete
	if complete {
		add(page.Signals.Headings, 3)
	}
	add(page.Description, 2)
	add(pagePath(page.URL), 1)
	if complete {
		add(page.Signals.Excerpt, 1)
	}
	keys := make([]string, 0, len(weights))
	for key := range weights {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		first, _ := utf8.DecodeRuneInString(key)
		p.Terms = append(p.Terms, LexicalTerm{key, wordFamily(key), string(first), utf8.RuneCountInString(key), weights[key], fuzzyWord(key)})
	}
	for phrase := range phrases {
		p.Phrases = append(p.Phrases, phrase)
	}
	sort.Strings(p.Phrases)
	return p, ctx.Err()
}

type LexicalDictionary interface {
	Forms(context.Context, string) ([]string, error)
	FuzzyBucket(context.Context, string, int) ([]string, error)
	Frequency(context.Context, []string) (int, error)
}

type QueryVariant struct {
	Index   int     `json:"index"`
	Term    string  `json:"term"`
	Kind    string  `json:"kind"`
	Quality float64 `json:"quality"`
	IDF     float64 `json:"idf"`
}

type IndexedQuery struct {
	Terms, Phrases, PhraseKeys []string
	Variants                   []QueryVariant
	TotalIDF                   float64
	CorpusPages                int
	Limited                    bool
}

func PrepareIndexedQuery(ctx context.Context, query string, count int, dictionary LexicalDictionary) (IndexedQuery, error) {
	q := IndexedQuery{Terms: terms(query), Variants: []QueryVariant{}, CorpusPages: count}
	words := sequence(query)
	for n := 2; n <= 3; n++ {
		for i := 0; i+n <= len(words) && len(q.Phrases) < 32; i++ {
			q.Phrases = append(q.Phrases, strings.Join(words[i:i+n], " "))
			q.PhraseKeys = append(q.PhraseKeys, phraseKey(words[i:i+n]))
		}
	}
	for i, term := range q.Terms {
		forms, err := dictionary.Forms(ctx, wordFamily(term))
		if err != nil {
			return q, err
		}
		variants := []QueryVariant{{Index: i, Term: term, Kind: "exact", Quality: 1}}
		for _, form := range forms {
			if form != term {
				variants = append(variants, QueryVariant{Index: i, Term: form, Kind: "morphology", Quality: .85})
			}
		}
		frequency := func() (int, error) {
			keys := make([]string, len(variants))
			for j, v := range variants {
				keys[j] = v.Term
			}
			return dictionary.Frequency(ctx, keys)
		}
		df, err := frequency()
		if err != nil {
			return q, err
		}
		if df == 0 && fuzzyWord(term) {
			first, _ := utf8.DecodeRuneInString(term)
			compared, found := 0, 0
		outer:
			for length := utf8.RuneCountInString(term) - 1; length <= utf8.RuneCountInString(term)+1; length++ {
				bucket, err := dictionary.FuzzyBucket(ctx, string(first), length)
				if err != nil {
					return q, err
				}
				for _, word := range bucket {
					if compared == maxFuzzyComparisons || found == 8 {
						q.Limited = true
						break outer
					}
					compared++
					if oneEditApart(term, word) {
						variants = append(variants, QueryVariant{Index: i, Term: word, Kind: "fuzzy", Quality: .65})
						found++
					}
				}
			}
			df, err = frequency()
			if err != nil {
				return q, err
			}
		}
		idf := 1 + math.Log(1+(float64(count-df)+.5)/(float64(df)+.5))
		q.TotalIDF += idf
		for j := range variants {
			variants[j].IDF = idf
		}
		q.Variants = append(q.Variants, variants...)
	}
	return q, ctx.Err()
}

type RankedCandidate struct {
	Page             Page
	Coverage         int
	Matching         Matching
	Matched, Missing []string
}

func ScoreIndexedPage(q IndexedQuery, page Page, profile LexicalProfile) RankedCandidate {
	r := RankedCandidate{Page: page, Matched: []string{}, Missing: []string{}, Matching: Matching{CorpusPages: q.CorpusPages, Limited: q.Limited || profile.Limited, Phrases: []string{}, Approximate: []TermMatch{}}}
	weights := map[string]int{}
	for _, term := range profile.Terms {
		weights[term.Term] = term.Weight
	}
	hits := make([]lexicalHit, len(q.Terms))
	idfs := make([]float64, len(q.Terms))
	for _, v := range q.Variants {
		idfs[v.Index] = v.IDF
		weight := weights[v.Term]
		old := hits[v.Index]
		if weight > 0 && (v.Quality > old.quality || v.Quality == old.quality && (weight > old.weight || weight == old.weight && v.Term < old.term)) {
			hits[v.Index] = lexicalHit{v.Term, v.Kind, v.Quality, weight}
		}
	}
	coverage, strength := 0.0, 0.0
	for i, term := range q.Terms {
		hit := hits[i]
		if hit.weight == 0 {
			r.Missing = append(r.Missing, term)
			continue
		}
		r.Matched = append(r.Matched, term)
		coverage += idfs[i] * hit.quality
		strength += idfs[i] * hit.quality * float64(hit.weight) / 4
		if hit.kind != "exact" {
			r.Matching.Approximate = append(r.Matching.Approximate, TermMatch{term, hit.term, hit.kind})
		}
	}
	for i, key := range q.PhraseKeys {
		if sort.SearchStrings(profile.Phrases, key) < len(profile.Phrases) && profile.Phrases[sort.SearchStrings(profile.Phrases, key)] == key {
			r.Matching.Phrases = append(r.Matching.Phrases, q.Phrases[i])
		}
	}
	if q.TotalIDF > 0 && len(q.Terms) > 0 {
		coverage /= q.TotalIDF
		strength /= q.TotalIDF
		relevance := .75*coverage + .25*strength
		if len(q.Phrases) > 0 {
			relevance = .65*coverage + .25*strength + .10*float64(len(r.Matching.Phrases))/float64(len(q.Phrases))
		}
		r.Coverage = int(math.Round(100 * float64(len(r.Matched)) / float64(len(q.Terms))))
		r.Matching.WeightedCoverage = int(math.Round(100 * coverage))
		r.Matching.Relevance = int(math.Round(100 * relevance))
	}
	return r
}

func ResultFromCandidates(query string, candidates []RankedCandidate) Result {
	r := Result{Query: query, Intent: intent(query), Level: "unknown", Matched: []string{}, Missing: []string{}, Alternatives: []Candidate{}, Checks: []Check{}, Gaps: []string{}, Warnings: []string{}}
	for _, c := range candidates {
		r.Alternatives = append(r.Alternatives, Candidate{TargetID: c.Page.TargetID, URL: c.Page.URL, Coverage: c.Coverage, Relevance: c.Matching.Relevance})
	}
	if len(candidates) == 0 || candidates[0].Coverage < 50 || candidates[0].Matching.WeightedCoverage < 50 {
		r.Missing = terms(query)
		r.Gaps = append(r.Gaps, "Відповідну сторінку не знайдено у вибраному аудиті. Перевірте повноту обходу та наявність цільової сторінки.")
		return r
	}
	best := candidates[0]
	id := best.Page.TargetID
	r.TargetID, r.TargetURL, r.Coverage = &id, best.Page.URL, best.Coverage
	r.Matched, r.Missing, r.Matching = best.Matched, best.Missing, &best.Matching
	r.Ambiguous = len(candidates) > 1 && candidates[1].Coverage >= 50 && candidates[1].Matching.WeightedCoverage >= 50 && candidates[1].Matching.Relevance >= best.Matching.Relevance-10
	if len(best.Matching.Approximate) > 0 {
		r.Warnings = append(r.Warnings, "Є збіги словоформ або друкарських помилок; перевірте запропоновану сторінку вручну.")
	}
	if best.Matching.Limited {
		r.Warnings = append(r.Warnings, "Застосовано ліміти локального індексу або наближеного пошуку; деякі терміни чи фрази могли не потрапити до зіставлення.")
	}
	if r.Ambiguous {
		r.Warnings = append(r.Warnings, "Є близькі кандидати. Це підстава для ручного порівняння, а не доказ канібалізації.")
	}
	if len(r.Missing) > 0 {
		r.Gaps = append(r.Gaps, "Перевірте покриття відсутніх термінів: "+strings.Join(r.Missing, ", "))
	}
	evaluate(&r, best.Page, terms(query))
	return r
}

func ResultForChosenCandidate(query string, c RankedCandidate) Result {
	id := c.Page.TargetID
	r := Result{Query: query, Intent: intent(query), TargetID: &id, TargetURL: c.Page.URL, Coverage: c.Coverage, Matching: &c.Matching, Level: "unknown", Matched: c.Matched, Missing: c.Missing, Alternatives: []Candidate{}, Checks: []Check{}, Gaps: []string{}, Warnings: []string{"Ціль обрано вручну; автоматичний результат збережено окремо."}}
	evaluate(&r, c.Page, terms(query))
	return r
}
