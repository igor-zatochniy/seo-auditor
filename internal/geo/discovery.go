package geo

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
)

const DiscoveryVersion = "sitemap-query-url-v1"
const MaxDiscoveryURLs = 5000
const maxDiscoveryWork = 2_000_000

type DiscoverySelection struct {
	URLs           []string
	MatchedQueries int
	Limited        bool
}

type discoveryURL struct {
	url, path string
	families  map[string]bool
	phrase    string
}

type discoveryHit struct {
	page  int
	score float64
}

// Відбір URL — евристика для discovery, не оцінка контенту чи AI-видимості.
func SelectDiscoveryURLs(ctx context.Context, queries, urls []string, limit int) (DiscoverySelection, error) {
	out := DiscoverySelection{URLs: []string{}}
	if len(urls) > MaxDiscoveryURLs || len(queries) > MaxQueries || limit < 0 || limit > 50 {
		return out, fmt.Errorf("discovery input exceeds budget")
	}
	ordered := append([]string(nil), urls...)
	sort.Strings(ordered)
	pages := []discoveryURL{}
	index := map[string][]int{}
	seen := map[string]bool{}
	for _, raw := range ordered {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 2048 || u.RawQuery != "" || u.ForceQuery || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		path, err := url.PathUnescape(u.EscapedPath())
		if err != nil {
			continue
		}
		words := sequence(path)
		if len(words) > 32 {
			words = words[:32]
			out.Limited = true
		}
		p := discoveryURL{url: raw, path: fold(path), families: map[string]bool{}, phrase: " " + phraseKey(words) + " "}
		for _, word := range words {
			p.families[wordFamily(word)] = true
		}
		for term := range p.families {
			index[term] = append(index[term], len(pages))
		}
		pages = append(pages, p)
	}
	best := make([]float64, len(pages))
	choices := [][]discoveryHit{}
	work := 0
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		words := sequence(query)
		families := map[string]bool{}
		for _, word := range words {
			families[wordFamily(word)] = true
		}
		keys := make([]string, 0, len(families))
		for term := range families {
			keys = append(keys, term)
		}
		sort.Strings(keys)
		scores := map[int]float64{}
		total := 0.0
		for _, term := range keys {
			posting := index[term]
			weight := 1 + math.Log(1+float64(len(pages)+1)/float64(len(posting)+1))
			total += weight
			for _, page := range posting {
				work++
				if work > maxDiscoveryWork {
					out.Limited = true
					break
				}
				scores[page] += weight
			}
			if work > maxDiscoveryWork {
				break
			}
		}
		if work > maxDiscoveryWork {
			break
		}
		hits := []discoveryHit{}
		for page, score := range scores {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			score = 100 * score / total
			for i := 0; i+1 < len(words) && i < 31; i++ {
				if strings.Contains(pages[page].phrase, " "+phraseKey(words[i:i+2])+" ") {
					score += 8
				}
			}
			score += discoveryPathBoost(pages[page].path, intent(query))
			best[page] = max(best[page], score)
			hits = append(hits, discoveryHit{page, score})
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return pages[hits[i].page].url < pages[hits[j].page].url
		})
		if len(hits) > 0 {
			out.MatchedQueries++
			choices = append(choices, hits[:min(3, len(hits))])
		}
	}
	selected := map[int]bool{}
	add := func(page int) {
		if !selected[page] && len(out.URLs) < limit {
			selected[page] = true
			out.URLs = append(out.URLs, pages[page].url)
		}
	}
	// Спочатку по одному найкращому кандидату на запит, щоб одна тема не зайняла весь бюджет.
	for round := 0; round < 3; round++ {
		for _, hits := range choices {
			if round < len(hits) {
				add(hits[round].page)
			}
		}
	}
	remaining := []discoveryHit{}
	for page, score := range best {
		if score > 0 {
			remaining = append(remaining, discoveryHit{page, score})
		}
	}
	sort.Slice(remaining, func(i, j int) bool {
		if remaining[i].score != remaining[j].score {
			return remaining[i].score > remaining[j].score
		}
		return pages[remaining[i].page].url < pages[remaining[j].page].url
	})
	for _, hit := range remaining {
		add(hit.page)
	}
	return out, ctx.Err()
}

func discoveryPathBoost(path, queryIntent string) float64 {
	segments := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '-' || r == '_' || r == '.' })
	commercial, information := false, false
	for _, segment := range segments {
		switch wordFamily(segment) {
		case "solution", "product", "service", "pric", "pricing", "compare", "comparison", "alternative", "platform", "рішення", "послуг":
			commercial = true
		case "blog", "guide", "doc", "learn", "article", "resource", "блог", "статт", "інструкц":
			information = true
		}
	}
	if (queryIntent == "commercial" || queryIntent == "transactional") && commercial || queryIntent == "informational" && information {
		return 12
	}
	return 0
}
