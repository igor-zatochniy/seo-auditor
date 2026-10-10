package geo

import (
	"context"
	"encoding/csv"
	"os"
	"strings"
	"testing"
)

// Синтетичний регресійний набір, не незалежна людська оцінка релевантності.
func TestLexicalBenchmark(t *testing.T) {
	f, err := os.Open("testdata/lexical-benchmark.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 121 {
		t.Fatalf("потрібно 120 контрольних запитів: %d", len(data)-1)
	}
	pages := []Page{}
	seen := map[string]bool{}
	queries := []string{}
	for _, row := range data[1:] {
		queries = append(queries, row[0])
		if !seen[row[1]] {
			seen[row[1]] = true
			pages = append(pages, Page{TargetID: int64(len(pages) + 1), URL: row[1], Title: row[0], H1: row[0], Description: row[0] + " guide examples comparison", Signals: &Signals{Version: 3, Complete: true, Excerpt: row[0] + " checklist recommendations explained"}})
		}
	}
	pages = append(pages, Page{TargetID: 99, URL: "https://example.com/navigation", Title: "guide examples comparison", Signals: &Signals{Version: 3, Complete: true, Excerpt: "guide examples comparison recommendations"}})
	results, err := Analyze(context.Background(), queries, pages)
	if err != nil {
		t.Fatal(err)
	}
	correct := 0
	for i, r := range results {
		if r.TargetURL == data[i+1][1] {
			correct++
		} else {
			t.Logf("ціль: %q → %q, очікується %q", r.Query, r.TargetURL, data[i+1][1])
		}
	}
	topics, err := ClusterTopics(context.Background(), results)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[int]int{}
	for _, topic := range topics {
		for _, q := range topic.Queries {
			groups[q.Ordinal] = topic.ID
		}
	}
	tp, fp, fn := 0, 0, 0
	for i := 1; i < len(data); i++ {
		for j := i + 1; j < len(data); j++ {
			want, got := data[i][2] == data[j][2], groups[i] == groups[j]
			if want && got {
				tp++
			}
			if !want && got {
				fp++
			}
			if want && !got {
				fn++
			}
		}
	}
	precision, recall := float64(tp)/float64(tp+fp), float64(tp)/float64(tp+fn)
	f1 := 2 * precision * recall / (precision + recall)
	accuracy := float64(correct) / float64(len(results))
	t.Logf("Синтетичний benchmark: %s / %s; top-1 %.3f; pairwise precision %.3f recall %.3f F1 %.3f; %d запитів", Model, TopicModel, accuracy, precision, recall, f1, len(results))
	if accuracy < 0.9 || f1 < 0.85 || strings.TrimSpace(Model) == "" {
		t.Fatal("регресія контрольного набору")
	}
}
