package geo

import (
	"context"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/semantic"
)

type semanticDictionary struct{}

func (semanticDictionary) Forms(context.Context, string) ([]string, error) { return nil, nil }
func (semanticDictionary) FuzzyBucket(context.Context, string, int) ([]string, error) {
	return nil, nil
}
func (semanticDictionary) Frequency(context.Context, []string) (int, error) { return 1, nil }

func TestSemanticKeepsLexicalSelectionAndExplainsCrossLanguage(t *testing.T) {
	page := Page{TargetID: 5, URL: "https://example.com/stripe", Title: "Stripe payment protection", H1: "Stripe payment protection"}
	profile, err := BuildLexicalProfile(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"Stripe payment protection", "захист платежів Stripe", "защита платежей Stripe", "Stripe ochrona płatności", "Stripe protección pagos", "Stripe protecție plăți"} {
		t.Run(query, func(t *testing.T) {
			q, units, err := PrepareSemanticQuery(t.Context(), query, 10, semanticDictionary{})
			if err != nil {
				t.Fatal(err)
			}
			candidate := ScoreIndexedPage(q, page, profile)
			r := CombineSemantic(query, nil, []RankedCandidate{candidate}, units, q.Limited)
			if r.TargetID == nil || *r.TargetID != 5 || r.Semantic == nil || !r.Semantic.Selected || r.Lexical == nil || r.Lexical.TargetID != nil || r.Semantic.Reason == "" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestSemanticDoesNotReplaceAmbiguousOrLimitedTargets(t *testing.T) {
	u, _ := semantic.Extract("payment protection")
	a := RankedCandidate{Page: Page{TargetID: 1, URL: "https://example.com/a"}, Coverage: 100, Matching: Matching{Relevance: 82, WeightedCoverage: 82}}
	b := a
	b.Page.TargetID = 2
	b.Page.URL = "https://example.com/b"
	for _, r := range []Result{CombineSemantic("payment protection", nil, []RankedCandidate{a, b}, u, false), CombineSemantic("payment protection", nil, []RankedCandidate{a}, u, true)} {
		if r.TargetID != nil || r.Semantic.Selected {
			t.Fatal("Неоднозначну ціль призначено автоматично", r)
		}
	}
}

func TestSemanticTopicClusteringKeepsDefaultBehavior(t *testing.T) {
	rows := []Result{{Query: "payment protection"}, {Query: "захист платежів"}}
	plain, err := ClusterTopics(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].Semantic = &SemanticAssessment{Model: semantic.Model}
	}
	translated, err := ClusterTopics(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || len(translated) != 1 {
		t.Fatal(len(plain), len(translated))
	}
}
