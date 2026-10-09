package geo

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestQueryMappingUsesIDFAndPhrases(t *testing.T) {
	pages := []Page{{TargetID: 1, Title: "Payment services"}, {TargetID: 2, Title: "Stripe integration"}, {TargetID: 3, Title: "Payment gateway"}, {TargetID: 4, Title: "Payment processing"}}
	r, err := Analyze(context.Background(), []string{"payment stripe"}, pages)
	if err != nil || r[0].TargetID == nil || *r[0].TargetID != 2 || r[0].Matching.WeightedCoverage <= 50 {
		t.Fatalf("IDF не виділив рідкісний термін: %+v %v", r, err)
	}
	pages = []Page{{TargetID: 1, Title: "Protection chargeback"}, {TargetID: 2, Title: "Chargeback protection"}}
	r, err = Analyze(context.Background(), []string{"chargeback protection"}, pages)
	if err != nil || r[0].TargetID == nil || *r[0].TargetID != 2 || len(r[0].Matching.Phrases) == 0 || r[0].Alternatives[0].Relevance <= r[0].Alternatives[1].Relevance {
		t.Fatalf("Фраза не вплинула на порядок: %+v %v", r, err)
	}
}

func TestQueryMappingExplainsMorphologyAndTypos(t *testing.T) {
	for _, tc := range []struct{ query, title, kind string }{
		{"prevent chargebacks", "Chargeback prevention", "morphology"},
		{"захист платежу", "Захист платежів", "morphology"},
		{"захист чарджбеку", "Захист чарджбеків", "morphology"},
		{"chargebak protection", "Chargeback protection", "fuzzy"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r, err := Analyze(context.Background(), []string{tc.query}, []Page{{TargetID: 1, Title: tc.title}})
			if err != nil || r[0].TargetID == nil || len(r[0].Matching.Approximate) == 0 || r[0].Matching.Approximate[0].Kind != tc.kind {
				t.Fatalf("Немає пояснення: %+v %v", r, err)
			}
		})
	}
	for _, query := range []string{"sku12346", "cat", "zzchargeback"} {
		r, err := Analyze(context.Background(), []string{query}, []Page{{TargetID: 1, Title: "sku12345 car chargeback"}})
		if err != nil || r[0].TargetID != nil {
			t.Fatalf("Небезпечний наближений збіг для %q: %+v", query, r)
		}
	}
}

func TestLexicalIndexBudgetsAndDeterminism(t *testing.T) {
	words := []string{}
	for i := range 500 {
		words = append(words, fmt.Sprintf("term%d", i))
	}
	c, err := buildLexicalCorpus(context.Background(), []Page{{TargetID: 1, Title: strings.Join(words, " ")}})
	if err != nil || !c.pages[0].limited || len(c.index) > maxPageTerms || len(c.pages[0].phrases) > maxPagePhrases {
		t.Fatalf("Ліміти не діють: %v", err)
	}
	for i := range 500 {
		if _, err := c.resolve(context.Background(), fmt.Sprintf("missing%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.cache) > 256 {
		t.Fatal("Необмежений кеш термінів")
	}
	pages := []Page{{TargetID: 2, Title: "Stripe payments"}, {TargetID: 1, Title: "Stripe payments"}}
	for range 20 {
		r, err := Analyze(context.Background(), []string{"stripe payments"}, pages)
		if err != nil || *r[0].TargetID != 1 || !r[0].Ambiguous {
			t.Fatal("Нестабільний порядок кандидатів")
		}
	}
}
