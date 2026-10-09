package geo

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxPageTerms        = 256
	maxPagePhrases      = 128
	maxFuzzyComparisons = 256
)

type TermMatch struct {
	Query string `json:"query"`
	Term  string `json:"term"`
	Kind  string `json:"kind"`
}

type Matching struct {
	Relevance        int         `json:"relevance"`
	WeightedCoverage int         `json:"weighted_coverage"`
	CorpusPages      int         `json:"corpus_pages"`
	Phrases          []string    `json:"phrases"`
	Approximate      []TermMatch `json:"approximate"`
	Limited          bool        `json:"limited"`
}

type lexicalPage struct {
	phrases map[string]bool
	limited bool
}
type lexicalBucket struct {
	first  rune
	length int
}
type lexicalCorpus struct {
	index   map[string][]posting
	forms   map[string][]string
	buckets map[lexicalBucket][]string
	pages   []lexicalPage
	ids     []int64
	cache   map[string]resolvedTerm
}
type lexicalHit struct {
	term, kind string
	quality    float64
	weight     int
}
type resolvedTerm struct {
	hits    map[int]lexicalHit
	idf     float64
	limited bool
}
type rankedPage struct {
	page, coverage   int
	matching         Matching
	matched, missing []string
}

func sequence(text string) []string {
	words := strings.FieldsFunc(fold(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	out := make([]string, 0, min(len(words), maxPageTerms))
	for _, word := range words {
		if utf8.RuneCountInString(word) < 2 || utf8.RuneCountInString(word) > 64 {
			continue
		}
		stop := false
		for _, s := range stopWords {
			if word == s {
				stop = true
				break
			}
		}
		if !stop {
			out = append(out, word)
		}
		if len(out) == maxPageTerms {
			break
		}
	}
	return out
}

// Обмежені словоформи, не повноцінна лематизація чи розуміння синонімів.
func wordFamily(word string) string {
	switch word {
	case "prevent", "prevents", "prevented", "preventing", "prevention":
		return "prevent"
	case "protect", "protects", "protected", "protecting", "protection":
		return "protect"
	case "платіж", "платежі", "платежів", "платежами", "платежу", "платежем":
		return "платіж"
	}
	latin, ukrainian := true, true
	for _, r := range word {
		latin = latin && r >= 'a' && r <= 'z'
		ukrainian = ukrainian && strings.ContainsRune("абвгґдеєжзиіїйклмнопрстуфхцчшщьюя", r)
	}
	if latin && len(word) >= 5 {
		if strings.HasSuffix(word, "ies") && len(word) >= 7 {
			return strings.TrimSuffix(word, "ies") + "y"
		}
		for _, suffix := range []string{"ing", "ed", "s"} {
			if strings.HasSuffix(word, suffix) && len(word)-len(suffix) >= 4 && !strings.HasSuffix(word, "ss") {
				return strings.TrimSuffix(word, suffix)
			}
		}
	}
	if ukrainian {
		for _, suffix := range []string{"іями", "ами", "ями", "ові", "ого", "ому", "ими", "ією", "іям", "іях", "ів", "ам", "ах", "ою", "єю", "ом", "ем", "а", "я", "и", "і", "у", "ю"} {
			if strings.HasSuffix(word, suffix) && utf8.RuneCountInString(word)-utf8.RuneCountInString(suffix) >= 4 {
				return strings.TrimSuffix(word, suffix)
			}
		}
	}
	return word
}

func phraseKey(words []string) string {
	parts := make([]string, len(words))
	for i, word := range words {
		parts[i] = wordFamily(word)
	}
	return strings.Join(parts, " ")
}

func pagePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path, _ := url.PathUnescape(u.EscapedPath())
	return path
}

func buildLexicalCorpus(ctx context.Context, pages []Page) (*lexicalCorpus, error) {
	c := &lexicalCorpus{index: map[string][]posting{}, forms: map[string][]string{}, buckets: map[lexicalBucket][]string{}, pages: make([]lexicalPage, len(pages)), ids: make([]int64, len(pages)), cache: map[string]resolvedTerm{}}
	for i, page := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		doc := lexicalPage{phrases: map[string]bool{}}
		weights := map[string]int{}
		add := func(text string, weight int) {
			words := sequence(text)
			if len(words) == maxPageTerms {
				doc.limited = true
			}
			for _, term := range words {
				if _, exists := weights[term]; !exists && len(weights) == maxPageTerms {
					doc.limited = true
					continue
				}
				weights[term] = max(weights[term], weight)
			}
			for n := 2; n <= 3; n++ {
				for j := 0; j+n <= len(words); j++ {
					key := phraseKey(words[j : j+n])
					if len(doc.phrases) < maxPagePhrases {
						doc.phrases[key] = true
					} else if !doc.phrases[key] {
						doc.limited = true
					}
				}
			}
		}
		add(page.Title, 4)
		add(page.H1, 4)
		if page.Signals != nil && page.Signals.Complete && !page.Signals.ContentIncomplete {
			add(page.Signals.Headings, 3)
		}
		add(page.Description, 2)
		add(pagePath(page.URL), 1)
		if page.Signals != nil && page.Signals.Complete && !page.Signals.ContentIncomplete {
			add(page.Signals.Excerpt, 1)
		}
		keys := make([]string, 0, len(weights))
		for key := range weights {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			c.index[key] = append(c.index[key], posting{i, weights[key]})
		}
		c.pages[i] = doc
		c.ids[i] = page.TargetID
	}
	keys := make([]string, 0, len(c.index))
	for key := range c.index {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		family := wordFamily(key)
		c.forms[family] = append(c.forms[family], key)
		if fuzzyWord(key) {
			first, _ := utf8.DecodeRuneInString(key)
			bucket := lexicalBucket{first, utf8.RuneCountInString(key)}
			c.buckets[bucket] = append(c.buckets[bucket], key)
		}
	}
	return c, nil
}

func fuzzyWord(word string) bool {
	if utf8.RuneCountInString(word) < 5 {
		return false
	}
	return !strings.ContainsFunc(word, func(r rune) bool { return !unicode.IsLetter(r) })
}

func oneEditApart(a, b string) bool {
	x, y := []rune(a), []rune(b)
	if abs(len(x)-len(y)) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(x) && j < len(y) {
		if x[i] == y[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(x) >= len(y) {
			i++
		}
		if len(y) >= len(x) {
			j++
		}
	}
	return edits+len(x)-i+len(y)-j == 1
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (c *lexicalCorpus) resolve(ctx context.Context, term string) (resolvedTerm, error) {
	if cached, ok := c.cache[term]; ok {
		return cached, nil
	}
	r := resolvedTerm{hits: map[int]lexicalHit{}}
	add := func(word, kind string, quality float64) {
		for _, p := range c.index[word] {
			old := r.hits[p.page]
			if quality > old.quality || quality == old.quality && (p.weight > old.weight || p.weight == old.weight && word < old.term) {
				r.hits[p.page] = lexicalHit{word, kind, quality, p.weight}
			}
		}
	}
	add(term, "exact", 1)
	for _, form := range c.forms[wordFamily(term)] {
		if form != term {
			add(form, "morphology", .85)
		}
	}
	// Наближений пошук лише за відсутності точного збігу та словоформ у корпусі.
	if len(r.hits) == 0 && fuzzyWord(term) {
		first, _ := utf8.DecodeRuneInString(term)
		compared, found := 0, 0
	outer:
		for length := utf8.RuneCountInString(term) - 1; length <= utf8.RuneCountInString(term)+1; length++ {
			for _, word := range c.buckets[lexicalBucket{first, length}] {
				if err := ctx.Err(); err != nil {
					return r, err
				}
				if compared == maxFuzzyComparisons || found == 8 {
					r.limited = true
					break outer
				}
				compared++
				if oneEditApart(term, word) {
					add(word, "fuzzy", .65)
					found++
				}
			}
		}
	}
	r.idf = 1 + math.Log(1+(float64(len(c.pages)-len(r.hits))+.5)/(float64(len(r.hits))+.5))
	if len(c.cache) < 256 {
		c.cache[term] = r
	}
	return r, nil
}

func (c *lexicalCorpus) rank(ctx context.Context, query string, qt []string) ([]rankedPage, error) {
	resolved := make([]resolvedTerm, len(qt))
	totalIDF := 0.0
	for i, term := range qt {
		var err error
		resolved[i], err = c.resolve(ctx, term)
		if err != nil {
			return nil, err
		}
		totalIDF += resolved[i].idf
	}
	if totalIDF == 0 {
		return nil, nil
	}
	words, phrases := sequence(query), []string{}
	phraseKeys := []string{}
	for n := 2; n <= 3; n++ {
		for i := 0; i+n <= len(words) && len(phrases) < 32; i++ {
			phrases = append(phrases, strings.Join(words[i:i+n], " "))
			phraseKeys = append(phraseKeys, phraseKey(words[i:i+n]))
		}
	}
	type pageScore struct{ page, coverage, weighted, relevance int }
	scores := make([]pageScore, 0, len(c.pages))
	for page := range c.pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		coverage, strength := 0.0, 0.0
		matched := 0
		for _, term := range resolved {
			hit, ok := term.hits[page]
			if !ok {
				continue
			}
			matched++
			coverage += term.idf * hit.quality
			strength += term.idf * hit.quality * float64(hit.weight) / 4
		}
		if matched == 0 {
			continue
		}
		coverage /= totalIDF
		strength /= totalIDF
		phraseCount := 0
		for _, key := range phraseKeys {
			if c.pages[page].phrases[key] {
				phraseCount++
			}
		}
		relevance := .75*coverage + .25*strength
		if len(phrases) > 0 {
			relevance = .65*coverage + .25*strength + .10*float64(phraseCount)/float64(len(phrases))
		}
		scores = append(scores, pageScore{page, int(math.Round(100 * float64(matched) / float64(len(qt)))), int(math.Round(100 * coverage)), int(math.Round(100 * relevance))})
	}
	sort.Slice(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		if a.relevance != b.relevance {
			return a.relevance > b.relevance
		}
		if a.weighted != b.weighted {
			return a.weighted > b.weighted
		}
		return c.ids[a.page] < c.ids[b.page]
	})
	ranked := make([]rankedPage, 0, min(3, len(scores)))
	for _, score := range scores[:min(3, len(scores))] {
		r := rankedPage{page: score.page, coverage: score.coverage, matched: []string{}, missing: []string{}, matching: Matching{Relevance: score.relevance, WeightedCoverage: score.weighted, CorpusPages: len(c.pages), Phrases: []string{}, Approximate: []TermMatch{}, Limited: c.pages[score.page].limited}}
		for i, term := range qt {
			r.matching.Limited = r.matching.Limited || resolved[i].limited
			hit, ok := resolved[i].hits[score.page]
			if !ok {
				r.missing = append(r.missing, term)
				continue
			}
			r.matched = append(r.matched, term)
			if hit.kind != "exact" {
				r.matching.Approximate = append(r.matching.Approximate, TermMatch{term, hit.term, hit.kind})
			}
		}
		for i, key := range phraseKeys {
			if c.pages[score.page].phrases[key] {
				r.matching.Phrases = append(r.matching.Phrases, phrases[i])
			}
		}
		ranked = append(ranked, r)
	}
	return ranked, nil
}
