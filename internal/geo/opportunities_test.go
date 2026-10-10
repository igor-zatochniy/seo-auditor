package geo

import (
	"context"
	"strings"
	"testing"
)

func opportunityFixture() PlanInput {
	s := func() *Signals {
		return &Signals{Version: 3, Complete: true, Excerpt: "chargeback prevention software", FirstParagraph: "chargeback prevention software", HTTP: &HTTPObservation{Status: 200}, Search: SearchControls{GooglebotRules: Allowed, OAISearchBotRules: Allowed, GoogleIndexing: Allowed, GoogleSnippet: Allowed}}
	}
	id, status, zero := int64(1), 200, 0
	return PlanInput{Results: []Result{{Query: "chargeback prevention software", TargetID: &id, TargetURL: "https://example.com/solutions", Coverage: 100, Matching: &Matching{WeightedCoverage: 100, Relevance: 80}, Eligibility: EvaluateEligibility(s()), Checks: []Check{{Name: "Порівняння", Category: "answer", Recommendation: "Перевірити comparison block", Evidence: "Таблицю не виявлено"}}}}, Pages: []Page{{TargetID: 1, URL: "https://example.com/solutions", Title: "chargeback prevention software", Signals: s()}, {TargetID: 2, URL: "https://example.com/blog", Title: "chargeback prevention software guide", Signals: s()}}, Facts: []PageFacts{{TargetID: 1, URL: "https://example.com/solutions", Status: &status, ScanStatus: "completed", Inlinks: &zero}, {TargetID: 2, URL: "https://example.com/blog", Status: &status, ScanStatus: "completed", Inlinks: &zero}}, Edges: map[LinkPair]bool{}, GraphReady: true, GraphFull: true}
}
func TestOpportunityPlanUsesEvidenceAndExistingEdges(t *testing.T) {
	input := opportunityFixture()
	input.Results = append(input.Results, input.Results[0])
	p, err := BuildOpportunityPlan(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	content := 0
	for _, task := range p.Tasks {
		if task.Kind == "content" {
			content++
			if len(task.Evidence) == 0 {
				t.Fatal("task without evidence")
			}
		}
	}
	if content != 1 || len(p.Links) != 1 || p.Links[0].From != 2 || p.Links[0].To != 1 {
		t.Fatal(p)
	}
	input.Edges[LinkPair{2, 1}] = true
	p, err = BuildOpportunityPlan(context.Background(), input)
	if err != nil || len(p.Links) != 0 {
		t.Fatal(p, err)
	}
	input.Edges = map[LinkPair]bool{}
	input.GraphFull = false
	p, err = BuildOpportunityPlan(context.Background(), input)
	if err != nil || len(p.Links) != 1 || p.Links[0].Confidence.Level != "low" {
		t.Fatal(p, err)
	}
	input.Pages[1].URL = "https://example.com/blog?token=[REDACTED]"
	p, err = BuildOpportunityPlan(context.Background(), input)
	if err != nil || len(p.Links) != 0 {
		t.Fatal("redacted request suggested as a link", p, err)
	}
}
func TestOpportunityPlanDoesNotInventUnknownSignals(t *testing.T) {
	input := opportunityFixture()
	input.Results[0].Checks = nil
	input.Results[0].Eligibility = EvaluateEligibility(nil)
	input.GraphReady = false
	p, err := BuildOpportunityPlan(context.Background(), input)
	if err != nil || len(p.Tasks) != 0 || len(p.Warnings) == 0 {
		t.Fatal(p, err)
	}
	input.Results[0].Eligibility.GoogleAI.Status = Blocked
	p, err = BuildOpportunityPlan(context.Background(), input)
	if err != nil || len(p.Tasks) != 1 || p.Tasks[0].Priority != "high" || !strings.Contains(p.Tasks[0].Action, "доцільність") {
		t.Fatal(p, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = BuildOpportunityPlan(ctx, input); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestOpportunityPlanRetainsHighPrioritiesAtTaskLimit(t *testing.T) {
	input := opportunityFixture()
	input.GraphReady = false
	input.Results = nil
	for i := 0; i < 600; i++ {
		id := int64(i + 1)
		r := Result{Query: "test", TargetID: &id, Checks: []Check{{Name: "schema", Category: "schema", Recommendation: "Перевірити", Evidence: "Вибірка"}}}
		if i == 599 {
			r.Eligibility = &AIEligibility{GoogleAI: EligibilityCheck{Status: Blocked}}
		}
		input.Results = append(input.Results, r)
	}
	p, err := BuildOpportunityPlan(context.Background(), input)
	if err != nil || !p.Limited || len(p.Tasks) != 500 || p.Tasks[0].Priority != "high" {
		t.Fatal(len(p.Tasks), p.Limited, err)
	}
}
func TestChosenTargetPreservesLowCoverageAndConfidence(t *testing.T) {
	input := opportunityFixture()
	r, err := AnalyzeChosenTarget(context.Background(), "unrelated dinosaurs", input.Pages, 2)
	if err != nil || r.TargetID == nil || *r.TargetID != 2 || r.Coverage != 0 || MatchingConfidence(r).Level != "unknown" {
		t.Fatal(r, err)
	}
	if _, err = AnalyzeChosenTarget(context.Background(), "test", input.Pages, 999); err == nil {
		t.Fatal("foreign target accepted")
	}
}
func TestImpactDoesNotMixMethodsKeysOrVersions(t *testing.T) {
	id := int64(1)
	r := Result{Query: "test", TargetID: &id, TargetURL: "https://example.com/?token=[REDACTED]", Matching: &Matching{Relevance: 60, CorpusPages: 2}}
	a := ImpactSample{Result: r, Mode: "automatic", KeyID: "key1", Fingerprint: "a"}
	b := a
	b.Result.Matching = &Matching{Relevance: 80, CorpusPages: 2}
	compare := func(x, y ImpactSample, old, new string) ImpactChange {
		return CompareImpact(old, new, []ImpactSample{x}, []ImpactSample{y})[0]
	}
	if c := compare(a, b, Model, Model); !c.Comparable || c.RelevanceDelta == nil || *c.RelevanceDelta != 20 {
		t.Fatal(c)
	}
	for _, variant := range []string{"model", "mode", "key", "target", "corpus", "limited"} {
		x := b
		old := Model
		switch variant {
		case "model":
			old = "local-lexical-v3"
		case "mode":
			x.Mode = "manual"
		case "key":
			x.KeyID = "key2"
		case "target":
			x.Fingerprint = "b"
		case "corpus":
			x.Result.Matching = &Matching{Relevance: 80, CorpusPages: 3}
		case "limited":
			x.Result.Matching = &Matching{Relevance: 80, CorpusPages: 2, Limited: true}
		}
		c := compare(a, x, old, Model)
		if c.Comparable || c.RelevanceDelta != nil {
			t.Fatal(variant, c)
		}
		if variant == "target" && (c.TargetChanged == nil || !*c.TargetChanged) {
			t.Fatal("redacted URL collision", c)
		}
	}
}
