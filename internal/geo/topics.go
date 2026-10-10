package geo

import (
	"context"
	"errors"
	"math"
	"sort"
)

const TopicModel = "local-topic-v1"

type TopicQuery struct {
	Ordinal   int    `json:"ordinal"`
	Query     string `json:"query"`
	State     string `json:"state"`
	TargetURL string `json:"target_url"`
	Relevance *int   `json:"relevance"`
}

type TopicURL struct {
	URL     string `json:"url"`
	Queries int    `json:"queries"`
}

type Topic struct {
	ID          int          `json:"id"`
	Label       string       `json:"label"`
	Terms       []string     `json:"terms"`
	Covered     int          `json:"covered"`
	Partial     int          `json:"partial"`
	NoTarget    int          `json:"no_target"`
	MainURL     string       `json:"main_url"`
	URLs        []TopicURL   `json:"urls"`
	Overlapping []TopicURL   `json:"overlapping_candidates"`
	Gaps        []string     `json:"gaps"`
	Queries     []TopicQuery `json:"queries"`
}

type topicVector map[string]float64

// Complete-link не дозволяє одному загальному запиту об'єднати різні теми транзитивним ланцюгом.
func ClusterTopics(ctx context.Context, results []Result) ([]Topic, error) {
	if len(results) > MaxQueries {
		return nil, errors.New("перевищено ліміт 1000 запитів")
	}
	vectors := make([]topicVector, len(results))
	df := map[string]int{}
	for i, r := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v := topicVector{}
		for _, term := range terms(r.Query) {
			v[wordFamily(term)] = 1
			if len(v) >= 24 {
				break
			}
		}
		vectors[i] = v
		for term := range v {
			df[term]++
		}
	}
	for _, v := range vectors {
		sum := 0.0
		for term := range v {
			w := 1 + math.Log(1+float64(len(results))/float64(df[term]))
			v[term] = w
			sum += w * w
		}
		if sum > 0 {
			for t := range v {
				v[t] /= math.Sqrt(sum)
			}
		}
	}
	order := make([]int, len(results))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := QueryKey(results[order[i]].Query), QueryKey(results[order[j]].Query)
		if a == b {
			return order[i] < order[j]
		}
		return a < b
	})
	groups := [][]int{}
	for _, index := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chosen, best := -1, 0.0
		for g, members := range groups {
			minimum := 1.0
			for _, other := range members {
				score, shared := topicSimilarity(vectors[index], vectors[other])
				if shared < 2 && !(len(vectors[index]) == 1 && len(vectors[other]) == 1 && shared == 1) {
					minimum = 0
					break
				}
				minimum = math.Min(minimum, score)
				if minimum < 0.45 {
					break
				}
			}
			if minimum >= 0.45 && minimum > best {
				chosen, best = g, minimum
			}
		}
		if chosen < 0 {
			groups = append(groups, []int{index})
		} else {
			groups[chosen] = append(groups[chosen], index)
		}
	}
	topics := make([]Topic, 0, len(groups))
	for g, members := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t := Topic{ID: g + 1, Label: results[members[0]].Query, Terms: []string{}, URLs: []TopicURL{}, Overlapping: []TopicURL{}, Gaps: []string{}, Queries: []TopicQuery{}}
		urls, overlap, keywords, gaps := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		sort.Ints(members)
		for _, i := range members {
			r := results[i]
			state := "no_target"
			var relevance *int
			if r.TargetID != nil {
				state = "partial"
				if r.Matching != nil {
					n := r.Matching.Relevance
					relevance = &n
					if r.Coverage >= 80 && r.Matching.WeightedCoverage >= 80 && !r.Ambiguous && !r.Matching.Limited {
						state = "covered"
					}
				}
				urls[r.TargetURL]++
			}
			switch state {
			case "covered":
				t.Covered++
			case "partial":
				t.Partial++
			default:
				t.NoTarget++
			}
			t.Queries = append(t.Queries, TopicQuery{Ordinal: i + 1, Query: r.Query, State: state, TargetURL: r.TargetURL, Relevance: relevance})
			for term := range vectors[i] {
				keywords[term]++
			}
			for _, gap := range r.Gaps {
				gaps[gap]++
			}
			if r.Ambiguous && r.Matching != nil {
				seen := map[string]bool{}
				for _, a := range r.Alternatives {
					if a.URL != r.TargetURL && a.Coverage >= 50 && a.Relevance >= r.Matching.Relevance-10 && !seen[a.URL] {
						overlap[a.URL]++
						seen[a.URL] = true
					}
				}
			}
		}
		t.URLs = rankTopicURLs(urls)
		if len(t.URLs) > 0 {
			t.MainURL = t.URLs[0].URL
		}
		t.Overlapping = rankTopicURLs(overlap)
		t.Terms = rankTopicText(keywords, 6)
		t.Gaps = rankTopicText(gaps, 5)
		topics = append(topics, t)
	}
	sort.SliceStable(topics, func(i, j int) bool {
		if len(topics[i].Queries) != len(topics[j].Queries) {
			return len(topics[i].Queries) > len(topics[j].Queries)
		}
		return QueryKey(topics[i].Label) < QueryKey(topics[j].Label)
	})
	return topics, nil
}

func topicSimilarity(a, b topicVector) (float64, int) {
	score, shared := 0.0, 0
	for term, w := range a {
		if other, ok := b[term]; ok {
			score += w * other
			shared++
		}
	}
	return score, shared
}
func rankTopicURLs(counts map[string]int) []TopicURL {
	out := []TopicURL{}
	for url, n := range counts {
		out = append(out, TopicURL{url, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Queries != out[j].Queries {
			return out[i].Queries > out[j].Queries
		}
		return out[i].URL < out[j].URL
	})
	return out
}
func rankTopicText(counts map[string]int, limit int) []string {
	out := []string{}
	for s := range counts {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out[:min(limit, len(out))]
}
