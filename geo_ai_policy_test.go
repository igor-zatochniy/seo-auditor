package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestCompareAIPoliciesSeparatesDesiredMismatchAndObservedDrift(t *testing.T) {
	current, _ := json.Marshal([]geo.AIControl{
		{Token: "Googlebot", Status: geo.Blocked, Confidence: "robots_parsed"},
		{Token: "GPTBot", Status: geo.Unknown, Confidence: "unknown"},
	})
	previous, _ := json.Marshal([]geo.AIControl{
		{Token: "Googlebot", Status: geo.Allowed, Confidence: "robots_parsed"},
		{Token: "GPTBot", Status: geo.Blocked, Confidence: "robots_parsed"},
	})
	results := compareAIPolicies(current, previous, map[string]string{"googlebot": geo.Allowed, "gptbot": geo.Unknown}, true)
	byToken := map[string]aiPolicyComparison{}
	for _, result := range results {
		byToken[result.Spec.Token] = result
	}
	if !byToken["Googlebot"].Mismatch || byToken["Googlebot"].Drift != "changed" || byToken["Googlebot"].PreviousStatus != geo.Allowed {
		t.Fatalf("desired mismatch and policy drift were conflated: %+v", byToken["Googlebot"])
	}
	if byToken["GPTBot"].Mismatch || byToken["GPTBot"].Drift != "not_comparable" {
		t.Fatalf("unknown current data was treated as blocked or stable: %+v", byToken["GPTBot"])
	}
	withoutBaseline := compareAIPolicies(current, nil, map[string]string{}, false)
	if withoutBaseline[0].Drift != "no_baseline" {
		t.Fatalf("no-baseline state is wrong: %+v", withoutBaseline[0])
	}
}

func TestSafeRobotsDirectiveRedactsQueryValues(t *testing.T) {
	target, err := url.Parse("https://example.com/report?api_key=page-secret")
	if err != nil {
		t.Fatal(err)
	}
	got := safeRobotsDirective(target, "Disallow: /private?access_token=robots-secret")
	if strings.Contains(got, "page-secret") || strings.Contains(got, "robots-secret") {
		t.Fatalf("directive leaked a query value: %q", got)
	}
	if !strings.HasPrefix(got, "Disallow: /private?") {
		t.Fatalf("directive lost its path evidence: %q", got)
	}
}
