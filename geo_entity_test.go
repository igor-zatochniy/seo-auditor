package main

import (
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/aicitation"
)

func TestValidateGEOEntityNormalizesAndBoundsRegistry(t *testing.T) {
	got, err := validateGEOEntity(aicitation.Entity{
		CanonicalName:   "Merchanto",
		EntityType:      "company",
		Aliases:         []string{"Merchanto Inc.", " merchanto inc. "},
		VerifiedDomains: []string{"BÜCHER.de", "bücher.de."},
		SameAsURLs:      []string{"https://www.wikidata.org/entity/Q123"},
		AmbiguityRules:  []aicitation.AmbiguityRule{{Alias: "Merchanto Inc.", ContextTerms: []string{"payments"}}},
	}, "Fallback", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalName != "Merchanto" || len(got.Aliases) != 1 || len(got.VerifiedDomains) != 1 || got.VerifiedDomains[0] != "xn--bcher-kva.de" || got.SameAsURLs[0] != "https://www.wikidata.org/entity/Q123" {
		t.Fatalf("сутність некоректно нормалізована: %+v", got)
	}
}

func TestValidateGEOEntityRejectsUnsafeOrUnresolvableValues(t *testing.T) {
	invalid := []aicitation.Entity{
		{CanonicalName: "Brand", EntityType: "company", VerifiedDomains: []string{"https://brand.example/path"}},
		{CanonicalName: "Brand", EntityType: "company", VerifiedDomains: []string{"brand.example"}, SameAsURLs: []string{"https://id.example/entity?token=secret"}},
		{CanonicalName: "Brand", EntityType: "company", VerifiedDomains: []string{"brand.example"}, AmbiguityRules: []aicitation.AmbiguityRule{{Alias: "Unknown", ContextTerms: []string{"term"}}}},
		{CanonicalName: "Brand", EntityType: "unknown", VerifiedDomains: []string{"brand.example"}},
	}
	for i, entity := range invalid {
		if _, err := validateGEOEntity(entity, "", ""); err == nil {
			t.Fatalf("case %d: некоректну сутність прийнято", i)
		}
	}
}
