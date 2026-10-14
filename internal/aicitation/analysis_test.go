package aicitation

import (
	"strings"
	"testing"
)

func TestAnswerCitationsAndBrandBoundaries(t *testing.T) {
	a, err := Analyze("Merchanto and Chargeflow. See [source](https://example.com/report?token=private). <script>alert(1)</script>", "https://chargeflow.io/tools\nhttps://bücher.de/page", Brand{"Merchanto", "example.com"}, []Brand{{"Chargeflow", "chargeflow.io"}})
	if err != nil || !a.BrandMention || len(a.CompetitorMentions) != 1 || len(a.Citations) != 3 || a.Citations[0].Relation != "competitor" || a.Citations[1].Domain != "xn--bcher-kva.de" || a.Citations[2].Relation != "own" {
		t.Fatal(a, err)
	}
	if Mentions("NotMerchantoX", "Merchanto") || Mentions("foobar", "bar") {
		t.Fatal("Частковий збіг бренду")
	}
}

func TestAnswerParsingIsBoundedAndRejectsCredentials(t *testing.T) {
	a, err := Analyze("https://user:password@example.com/", "", Brand{}, nil)
	if err != nil || len(a.Citations) != 0 {
		t.Fatal(a, err)
	}
	a, err = Analyze(strings.Repeat("https://example.com/page ", 1000), "", Brand{}, nil)
	if err != nil || !a.Limited || len(a.Citations) > MaxCandidates {
		t.Fatal(a, err)
	}
	if _, err = Analyze(strings.Repeat("x", 100001), "", Brand{}, nil); err == nil {
		t.Fatal("Перевищення ліміту прийнято")
	}
}

func TestOversizedCitationIsExplicitlyLimited(t *testing.T) {
	a, err := Analyze("See https://example.com/report?token="+strings.Repeat("a", 4096), "", Brand{"Example", "example.com"}, nil)
	if err != nil || !a.Limited || len(a.Citations) != 0 {
		t.Fatal("Обрізання URL не позначено явно", a, err)
	}
}

func TestEntityAliasesUnicodeContextAndCitationIdentity(t *testing.T) {
	brand := Entity{
		CanonicalName:   "Apple",
		Aliases:         []string{"Apple Inc."},
		EntityType:      "company",
		VerifiedDomains: []string{"apple.com", "apple.example"},
		SameAsURLs:      []string{"https://www.wikidata.org/entity/Q312"},
		AmbiguityRules:  []AmbiguityRule{{Alias: "Apple", ContextTerms: []string{"iPhone", "Mac"}}},
	}
	ambiguous, err := AnalyzeEntities("I ate an Apple after lunch.", "https://notapple.com/article", brand, nil, nil)
	if err != nil || ambiguous.BrandMention || ambiguous.BrandEntity.Status != "ambiguous" || len(ambiguous.BrandEntity.AmbiguousAliases) != 1 || len(ambiguous.Citations) != 1 || ambiguous.Citations[0].Relation != "external" {
		t.Fatalf("неоднозначне слово не відокремлено: %+v, %v", ambiguous, err)
	}
	resolved, err := AnalyzeEntities("ＡＰＰＬＥ announced an iPhone update.", "https://sub.apple.example/news", brand, nil, nil)
	if err != nil || !resolved.BrandMention || resolved.BrandEntity.Status != "matched" || len(resolved.Citations) != 1 || resolved.Citations[0].Relation != "own" {
		t.Fatalf("NFKC/контекст/підтверджений домен не спрацювали: %+v, %v", resolved, err)
	}
	if !Mentions("Apple Inc. devices", "Apple Inc") || Mentions("pineapple is sweet", "apple") {
		t.Fatal("межі токенів порушено")
	}
	separateSentence, err := AnalyzeEntities("Apple. The article mentions an iPhone later.", "", brand, nil, nil)
	if err != nil || separateSentence.BrandEntity.Status != "ambiguous" {
		t.Fatalf("контекст з іншого речення помилково розв'язав назву: %+v, %v", separateSentence.BrandEntity, err)
	}
	identityLink, err := AnalyzeEntities("Profile: https://www.wikidata.org/entity/Q312", "", brand, nil, nil)
	if err != nil || !identityLink.BrandEntity.SameAsReferenced || identityLink.BrandMention {
		t.Fatalf("sameAs помилково змішався зі згадкою бренду: %+v, %v", identityLink.BrandEntity, err)
	}
}

func TestCitationAbsenceRequiresExplicitCompleteSources(t *testing.T) {
	partial, err := AnalyzeEntities("Merchanto was mentioned", "https://other.example/source", Entity{CanonicalName: "Merchanto", EntityType: "company", VerifiedDomains: []string{"merchanto.example"}}, nil, nil)
	if err != nil || partial.SourceCoverage != "provided_unreviewed" || partial.BrandEntity.Status != "matched" {
		t.Fatalf("аналіз не повинен вигадувати повноту джерел: %+v, %v", partial, err)
	}
	limited, err := Analyze(strings.Repeat("https://other.example/source ", 1000), "", Brand{}, nil)
	if err != nil || !limited.Limited || limited.SourceCoverage != "limited" {
		t.Fatalf("обмежену вибірку не позначено: %+v, %v", limited, err)
	}
}
