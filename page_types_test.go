package main

import (
	"encoding/json"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"strings"
	"testing"
	"time"
)

func typeState(p typedPage, code string) string {
	for _, v := range p.Checks {
		if v.Code == code {
			return v.State
		}
	}
	return ""
}
func TestPageTypesConservativeSignalsAndOverrides(t *testing.T) {
	http := 200
	collect := structureddata.NewCollector()
	collect.AddBlock([]byte(`{"@type":"Product","offers":{"price":"0","availability":"https://schema.org/OutOfStock"}}`))
	input := typePageInput{ID: 1, URL: "https://example.com/pl/item", Status: scanStatusCompleted, HTTP: &http, Schema: collect.Result("https://example.com/pl/item")}
	p := classifyPage(input, nil)
	if p.Type != "product" || typeState(p, "product_price") != "observed" || typeState(p, "product_availability") != "observed" {
		t.Fatalf("product=%+v", p)
	}
	input.Schema.Products.WithAvailability = nil
	if got := typeState(classifyPage(input, nil), "product_availability"); got != "unknown" {
		t.Fatalf("legacy availability=%s", got)
	}
	input.Schema.Types = append(input.Schema.Types, "Article")
	if p = classifyPage(input, nil); p.Type != "unknown" {
		t.Fatalf("nested schema classified as certain: %+v", p)
	}
	p = classifyPage(input, []pageTypeRule{{"/pl/*", "category"}, {"/*", "service"}})
	if p.Type != "category" || p.Source != "rule" {
		t.Fatalf("rules=%+v", p)
	}
	input.Override = "article"
	input.Status = "robots_blocked"
	p = classifyPage(input, nil)
	if p.Source != "manual" || len(p.Checks) != 1 || p.Checks[0].State != "unknown" {
		t.Fatalf("blocked=%+v", p)
	}
	input.Override = "auto"
	input.Status = scanStatusCompleted
	input.Schema = nil
	input.URL = "https://example.com/blog/test"
	if p = classifyPage(input, nil); p.Type != "article" || p.Confidence != "low" || typeState(p, "article_date") != "unknown" {
		t.Fatalf("path=%+v", p)
	}
	input.URL = "https://example.com/solutions/test"
	input.GEO = &geo.Signals{Blocks: &geo.BlockSample{Complete: false, Count: 0}}
	if got := typeState(classifyPage(input, nil), "service_answer"); got != "unknown" {
		t.Fatal(got)
	}
}

func TestRemediationMarkdownEscapesUntrustedContent(t *testing.T) {
	p := remediationPackage{ReportID: "fixture", GeneratedAt: time.Now(), Tasks: []remediationTask{{Task: geo.Opportunity{Action: "<script>alert(1)</script>\n# injected [link](javascript:evil)", TargetURL: "https://example.com/", Priority: "high"}, Acceptance: []string{"Перевірити"}, Recheck: map[string]any{"instruction": "Ручний повторний аудит"}}}}
	md := packageMarkdown(p)
	if strings.Contains(md, "<script>") || strings.Contains(md, "\n# injected") || strings.Contains(md, "[link]") {
		t.Fatal("markdown injection", md)
	}
	if !strings.Contains(md, "Критерії приймання") || !strings.Contains(md, "Окремого target-доказу немає") {
		t.Fatal(md)
	}
	var legacy structureddata.ProductEvidence
	if err := json.Unmarshal([]byte(`{"count":1,"with_price":1,"complete":true}`), &legacy); err != nil || legacy.WithAvailability != nil {
		t.Fatal("legacy evidence changed")
	}
}
