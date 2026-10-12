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
