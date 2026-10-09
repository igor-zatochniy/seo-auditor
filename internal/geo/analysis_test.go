package geo

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestValidationAndDomainBoundary(t *testing.T) {
	input := Input{Domain: "https://bücher.de/", Queries: " Stripe chargeback protection \nstripe chargeback protection\nЯк запобігти чарджбекам"}
	queries, err := Validate(&input)
	if err != nil || len(queries) != 2 || input.Domain != "xn--bcher-kva.de" {
		t.Fatalf("Некоректна нормалізація: %v %v", queries, err)
	}
	for _, bad := range []string{"https://user:secret@example.com", "127.0.0.1", "example.com/path", "example.com?token=secret", "-bad.example", "example.com:8080"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Errorf("Прийнято некоректний домен %q", bad)
		}
	}
	if !InDomain("https://shop.bücher.de/a", input.Domain) || InDomain("https://evilxn--bcher-kva.de/a", input.Domain) {
		t.Fatal("Порушено межі домену")
	}
	for _, queries := range []string{"", "test\x00value", strings.Repeat("я", 301)} {
		in := Input{Domain: "example.com", Queries: queries}
		if _, err := Validate(&in); err == nil {
			t.Fatal("Прийнято некоректні запити")
		}
	}
	var lines []string
	for i := range 1001 {
		lines = append(lines, fmt.Sprintf("запит %d", i))
	}
	input = Input{Domain: "example.com", Queries: strings.Join(lines[:1000], "\n")}
	if _, err := Validate(&input); err != nil {
		t.Fatal(err)
	}
	input.Queries = strings.Join(lines, "\n")
	if _, err := Validate(&input); err == nil {
		t.Fatal("Не застосовано ліміт")
	}
}

func TestMappingGapsAndUnknownEvidence(t *testing.T) {
	pages := []Page{
		{TargetID: 1, URL: "https://example.com/stripe", Title: "Stripe chargeback protection", H1: "Stripe chargeback protection", H1Count: 1,
			Signals: &Signals{Version: 1, Complete: true, FirstParagraph: "Stripe chargeback protection for merchants", HasList: true, SchemaTypes: []string{"Product"}}},
		{TargetID: 2, URL: "https://example.com/saas", Title: "SaaS payment automation"},
	}
	results, err := Analyze(context.Background(), []string{"Stripe chargeback protection", "how SaaS payment automation works", "banana recipes", "best chargeback protection"}, pages)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].TargetID == nil || *results[0].TargetID != 1 || results[0].Coverage != 100 || len(results[0].Categories) != 3 || results[0].Readiness != nil {
		t.Fatalf("Хибне зіставлення: %+v", results[0])
	}
	if results[1].Readiness != nil || len(results[1].Warnings) == 0 {
		t.Fatal("Старі дані помилково отримали оцінку")
	}
	if results[2].TargetID != nil || results[2].Level != "unknown" || len(results[2].Gaps) == 0 {
		t.Fatal("Вигадана цільова сторінка")
	}
	if results[3].Intent != "commercial" || len(results[3].Checks) != 7 || results[3].SharedQueries != 2 {
		t.Fatal("Не збережено пояснення комерційного запиту")
	}
}

func TestGEOCategoriesAndSearchRulesStayIndependent(t *testing.T) {
	s := &Signals{Version: 2, Complete: true, ContentSource: "main", FirstParagraph: "payment protection", HasList: true, HasAuthor: true,
		Search: SearchControls{GooglebotRules: Blocked, OAISearchBotRules: Allowed, GoogleIndexing: Allowed, GoogleSnippet: Blocked}}
	page := Page{TargetID: 1, Title: "payment protection", H1Count: 1, ExternalLinks: 1, Signals: s}
	results, err := Analyze(context.Background(), []string{"payment protection"}, []Page{page})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Readiness != nil || r.Level != "strong" || r.Categories[2].Passed != 0 || r.Search.GooglebotRules != Blocked || r.Search.OAISearchBotRules != Allowed || len(r.Warnings) == 0 {
		t.Fatalf("Правила або необов'язкова schema змінили оцінку контенту: %+v", r)
	}
	if len(r.Gaps) != 0 {
		t.Fatal("Необов'язкова розмітка подана як прогалина")
	}
	s.ContentIncomplete = true
	results, err = Analyze(context.Background(), []string{"payment protection"}, []Page{page})
	if err != nil || len(results[0].Categories) != 0 || results[0].Level != "unknown" {
		t.Fatal("Неповний контент отримав оцінку")
	}
}

func TestAmbiguityAndCancellation(t *testing.T) {
	pages := []Page{{TargetID: 2, URL: "https://example.com/b", Title: "Stripe chargeback"}, {TargetID: 1, URL: "https://example.com/a", Title: "Stripe chargeback"}}
	r, err := Analyze(context.Background(), []string{"Stripe chargeback"}, pages)
	if err != nil || !r[0].Ambiguous || *r[0].TargetID != 1 {
		t.Fatal("Нестабільний порядок кандидатів")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Analyze(ctx, []string{"Stripe"}, pages); err == nil {
		t.Fatal("Скасування проігнороване")
	}
}

func BenchmarkThousandQueriesAndPages(b *testing.B) {
	queries, pages := make([]string, 1000), make([]Page, 1000)
	for i := range 1000 {
		queries[i] = fmt.Sprintf("stripe protection %d", i)
		pages[i] = Page{TargetID: int64(i + 1), Title: queries[i], URL: fmt.Sprintf("https://example.com/%d", i)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Analyze(context.Background(), queries, pages); err != nil {
			b.Fatal(err)
		}
	}
}
