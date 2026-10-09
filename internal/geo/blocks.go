package geo

import "sort"

type BlockMatch struct {
	TextBlock
	Coverage int      `json:"coverage"`
	Matched  []string `json:"matched"`
}

// Лексичний збіг не доводить ні повноти відповіді, ні єдиного наміру абзацу.
func matchBlocks(sample *BlockSample, query []string) []BlockMatch {
	if sample == nil || len(query) == 0 {
		return nil
	}
	var matches []BlockMatch
	for _, block := range sample.Items {
		words := terms(block.Text)
		matched := []string{}
		for _, q := range query {
			for _, word := range words {
				if q == word {
					matched = append(matched, q)
					break
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		matches = append(matches, BlockMatch{TextBlock: block, Coverage: 100 * len(matched) / len(query), Matched: matched})
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Coverage > matches[j].Coverage })
	if len(matches) > 3 {
		matches = matches[:3]
	}
	return matches
}
