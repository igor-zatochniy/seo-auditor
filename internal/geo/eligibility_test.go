package geo

import "testing"

func TestAIEligibilityKeepsProviderRulesIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, google, chat string
		change             func(*Signals)
	}{
		{"allowed", Allowed, Allowed, func(*Signals) {}},
		{"google-noindex", Blocked, Allowed, func(s *Signals) { s.Search.GoogleIndexing = Blocked }},
		{"google-nosnippet", Blocked, Allowed, func(s *Signals) { s.Search.GoogleSnippet = Blocked }},
		{"snippet-zero", Blocked, Allowed, func(s *Signals) { n := 0; s.Search.MaxSnippet = &n }},
		{"snippet-limit", Limited, Allowed, func(s *Signals) { n := 100; s.Search.MaxSnippet = &n }},
		{"snippet-unlimited", Allowed, Allowed, func(s *Signals) { n := -1; s.Search.MaxSnippet = &n }},
		{"partial-snippet", Limited, Allowed, func(s *Signals) { s.Search.DataNoSnippet = true }},
		{"google-robots", Blocked, Allowed, func(s *Signals) { s.Search.GooglebotRules = Blocked }},
		{"search-robots", Allowed, Blocked, func(s *Signals) { s.Search.OAISearchBotRules = Blocked }},
		{"training-robots", Allowed, Allowed, func(s *Signals) { s.Search.GPTBotRules = Blocked }},
		{"unknown-search", Allowed, Unknown, func(s *Signals) { s.Search.OAISearchBotRules = Unknown }},
		{"http-failure", Blocked, Blocked, func(s *Signals) { s.HTTP.Status = 403 }},
		{"unknown-http", Unknown, Unknown, func(s *Signals) { s.HTTP = nil }},
		{"incomplete", Unknown, Unknown, func(s *Signals) { s.Complete = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Signals{Version: 3, Complete: true, HTTP: &HTTPObservation{Status: 200}, Search: SearchControls{GooglebotRules: Allowed, OAISearchBotRules: Allowed, GoogleIndexing: Allowed, GoogleSnippet: Allowed}}
			tc.change(s)
			got := EvaluateEligibility(s)
			if got.GoogleAI.Status != tc.google || got.ChatGPTSearch.Status != tc.chat || len(got.GoogleAI.Reasons) == 0 || len(got.ChatGPTSearch.Reasons) == 0 {
				t.Fatalf("Хибні передумови: %+v", got)
			}
		})
	}
	if got := EvaluateEligibility(nil); got.GoogleAI.Status != Unknown || got.ChatGPTSearch.Status != Unknown {
		t.Fatal("Відсутні дані подано як дозвіл")
	}
}
