package geo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func TestTopicClustersCoverageAndNonTransitiveMembership(t *testing.T) {
	id := int64(1)
	rows := []Result{
		{Query: "chargeback prevention software", TargetID: &id, TargetURL: "https://example.com/a", Coverage: 100, Matching: &Matching{Relevance: 90, WeightedCoverage: 100}},
		{Query: "chargeback prevention platform", TargetID: &id, TargetURL: "https://example.com/a", Coverage: 60, Matching: &Matching{Relevance: 60, WeightedCoverage: 60}},
		{Query: "chargeback prevention pricing", Gaps: []string{"Немає цілі"}},
		{Query: "Stripe integration guide"},
		{Query: "pricing software Stripe integration"},
	}
	topics, err := ClusterTopics(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	covered, partial, missing := 0, 0, 0
	for _, topic := range topics {
		covered += topic.Covered
		partial += topic.Partial
		missing += topic.NoTarget
		for _, a := range topic.Queries {
			for _, b := range topic.Queries {
				if a.Ordinal != b.Ordinal && ((a.Query == "Stripe integration guide" && b.Query == "chargeback prevention software") || (b.Query == "Stripe integration guide" && a.Query == "chargeback prevention software")) {
					t.Fatal("Різні теми об'єднано транзитивно")
				}
			}
		}
	}
	if covered != 1 || partial != 1 || missing != 3 {
		t.Fatalf("coverage=%d/%d/%d", covered, partial, missing)
	}
	if rows[0].Query != "chargeback prevention software" {
		t.Fatal("Вхідні результати змінено")
	}
	permuted := append([]Result(nil), rows...)
	for i, j := 0, len(permuted)-1; i < j; i, j = i+1, j-1 {
		permuted[i], permuted[j] = permuted[j], permuted[i]
	}
	other, err := ClusterTopics(t.Context(), permuted)
	if err != nil {
		t.Fatal(err)
	}
	membership := func(items []Topic) []string {
		out := []string{}
		for _, topic := range items {
			q := []string{}
			for _, r := range topic.Queries {
				q = append(q, r.Query)
			}
			sort.Strings(q)
			out = append(out, fmt.Sprint(q))
		}
		sort.Strings(out)
		return out
	}
	if !reflect.DeepEqual(membership(topics), membership(other)) {
		t.Fatal("Порядок запитів змінив кластери")
	}
}

func TestTopicClusteringIsBoundedAndCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ClusterTopics(ctx, []Result{{Query: "chargeback prevention"}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ClusterTopics(t.Context(), make([]Result, 1001)); err == nil {
		t.Fatal("Ліміт не перевірено")
	}
	rows := make([]Result, 1000)
	for i := range rows {
		rows[i].Query = fmt.Sprintf("chargeback prevention topic %d", i)
	}
	items, err := ClusterTopics(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, item := range items {
		total += len(item.Queries)
	}
	if total != 1000 {
		t.Fatal(total)
	}
}

func TestGEOChangesRequireKnownComparableSignals(t *testing.T) {
	id := int64(1)
	a := Result{Query: "chargeback protection", TargetID: &id, TargetURL: "https://example.com/a", Matching: &Matching{Relevance: 62}, Search: &SearchControls{OAISearchBotRules: Allowed}, Eligibility: &AIEligibility{GoogleAI: EligibilityCheck{Status: Allowed}, ChatGPTSearch: EligibilityCheck{Status: Allowed}}, Blocks: &BlockSample{Complete: true}, BlockMatches: []BlockMatch{{Coverage: 100}}, Entities: &EntityAssessment{Checks: []EntityCheck{{Key: "visible_brand", Status: "missing"}}}}
	b := a
	b.TargetURL = "https://example.com/b"
	b.Matching = &Matching{Relevance: 80}
	b.Search = &SearchControls{OAISearchBotRules: Blocked}
	b.Eligibility = &AIEligibility{GoogleAI: EligibilityCheck{Status: Unknown}, ChatGPTSearch: EligibilityCheck{Status: Blocked}}
	b.Blocks = &BlockSample{Complete: false}
	b.BlockMatches = nil
	b.Entities = &EntityAssessment{Checks: []EntityCheck{{Key: "visible_brand", Status: "found"}}}
	items := CompareResults([]Result{a}, []Result{b})
	kinds := map[string]bool{}
	for _, c := range items {
		kinds[c.Kind] = true
	}
	if len(items) != 5 || !kinds["target_url"] || !kinds["relevance"] || !kinds["brand_entity"] || kinds["answer_blocks"] || kinds["google_ai_eligibility"] {
		t.Fatalf("Неправильні зміни: %+v", items)
	}
	citations := CompareCitations(1, a.Query, []CitationObservation{{Engine: "chatgpt", Citation: "no"}, {Engine: "google_ai", Citation: "unknown"}}, []CitationObservation{{Engine: "chatgpt", Citation: "yes"}, {Engine: "google_ai", Citation: "no"}})
	if len(citations) != 1 || citations[0].Kind != "observed_citation:chatgpt" {
		t.Fatal(citations)
	}
}

func TestCompetitorGapsDoNotTurnUnknownIntoMissing(t *testing.T) {
	id := int64(1)
	own := Result{TargetID: &id, Checks: []Check{{Name: "Таблиця", Category: "structure", Passed: false}}, Entities: &EntityAssessment{Checks: []EntityCheck{{Key: "author", Name: "Автор", Status: Unknown}}}}
	other := Result{TargetID: &id, Checks: []Check{{Name: "Таблиця", Category: "structure", Passed: true, Evidence: "Виявлено table"}, {Name: "FAQ", Passed: true}}, Entities: &EntityAssessment{Checks: []EntityCheck{{Key: "author", Name: "Автор", Status: "found"}}}}
	if gaps := ComparePageSignals(own, other); len(gaps) != 1 || gaps[0].Signal != "Таблиця" {
		t.Fatal(gaps)
	}
	own.TargetID = nil
	if gaps := ComparePageSignals(own, other); len(gaps) != 0 {
		t.Fatal(gaps)
	}
}

func TestGEOChangesTrackEachKnownEntitySignal(t *testing.T) {
	a := Result{Query: "chargeback protection", Entities: &EntityAssessment{Checks: []EntityCheck{
		{Key: "organization", Status: "missing"},
		{Key: "same_as", Status: "missing"},
		{Key: "publisher", Status: "mismatch"},
		{Key: "author", Status: Unknown},
		{Key: "product_brand", Status: "not_applicable"},
	}}}
	b := Result{Query: a.Query, Entities: &EntityAssessment{Checks: []EntityCheck{
		{Key: "organization", Status: "found"},
		{Key: "same_as", Status: "found"},
		{Key: "publisher", Status: "found"},
		{Key: "author", Status: "found"},
		{Key: "product_brand", Status: "found"},
	}}}
	items := CompareResults([]Result{a}, []Result{b})
	if len(items) != 4 {
		t.Fatalf("Неправильні entity-події: %+v", items)
	}
	for _, item := range items {
		if item.Kind == "entity:author" || item.Kind == "brand_entity" || item.After != "found" {
			t.Fatalf("Неперевірений сигнал став зміною: %+v", item)
		}
	}
}

func TestGEOChangesIgnoreMissingLegacyRuleValues(t *testing.T) {
	a := Result{Query: "chargeback protection", Search: &SearchControls{}, Eligibility: &AIEligibility{}}
	b := Result{Query: a.Query, Search: &SearchControls{OAISearchBotRules: Allowed}, Eligibility: &AIEligibility{
		GoogleAI: EligibilityCheck{Status: Allowed}, ChatGPTSearch: EligibilityCheck{Status: Allowed},
	}}
	if items := CompareResults([]Result{a}, []Result{b}); len(items) != 0 {
		t.Fatalf("Відсутні legacy-значення стали спостереженими змінами: %+v", items)
	}
}
