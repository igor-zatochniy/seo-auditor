package geo

import (
	"context"
	"testing"
)

func TestBlockMatchingIsEvidenceNotVisibility(t *testing.T) {
	s := &Signals{Version: 2, Complete: true, Search: SearchControls{OAISearchBotRules: Allowed, GPTBotRules: Blocked}, Blocks: &BlockSample{Complete: true, Count: 2, Items: []TextBlock{{Text: "Stripe payment protection", Characters: 350}, {Text: "Other topic", Characters: 320}}}}
	pages := []Page{{TargetID: 1, URL: "https://example.com/stripe", Title: "Stripe payment protection", Signals: s}}
	results, err := Analyze(context.Background(), []string{"Stripe protection"}, pages)
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if len(r.BlockMatches) != 1 || r.BlockMatches[0].Coverage != 100 || r.Readiness != nil || r.Search.GPTBotRules != Blocked || r.Search.OAISearchBotRules != Allowed {
		t.Fatalf("bad evidence: %+v", r)
	}
	s.Search.GPTBotRules = Allowed
	other, _ := Analyze(context.Background(), []string{"Stripe protection"}, pages)
	if other[0].Level != r.Level {
		t.Fatal("Training permission affected search/content rating")
	}
	if matchBlocks(nil, []string{"test"}) != nil {
		t.Fatal("legacy result invented evidence")
	}
}
