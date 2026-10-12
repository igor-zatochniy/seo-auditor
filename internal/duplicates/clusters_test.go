package duplicates

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func testDocument(id int64, sha string, sim string) Document {
	return Document{TargetID: id, Signature: &Signature{Model: Model, SHA256: sha, SimHash: sim, Words: 200, Complete: true, Source: "main"}, KeyID: "test-key", TitleKey: "merchant protection", H1Key: "merchant protection", Canonical: "same-canonical"}
}

func TestExactNearAndUnrelatedClusters(t *testing.T) {
	d := NewDetector()
	d.Add(testDocument(1, strings.Repeat("a", 64), "0"))
	d.Add(testDocument(2, strings.Repeat("a", 64), "0"))
	d.Add(testDocument(3, strings.Repeat("b", 64), "100010001"))
	d.Add(testDocument(4, strings.Repeat("c", 64), "ffffffffffffffff"))
	s, c, err := d.Analyze(t.Context())
	if err != nil || s.ExactClusters != 1 || s.NearClusters != 1 || s.SimilarPages != 3 || len(c) != 1 || c[0].Count != 3 || !c[0].PotentialOverlap {
		t.Fatal(s, c, err)
	}
	if !s.NearSearchComplete || c[0].Canonical != "consistent" {
		t.Fatal(s, c)
	}
}

func TestRegionalProductAndKeyRotationAreQualified(t *testing.T) {
	for _, kind := range []string{"regional", "product", "rotation", "missing"} {
		t.Run(kind, func(t *testing.T) {
			d := NewDetector()
			a := testDocument(1, strings.Repeat("a", 64), "0")
			b := testDocument(2, strings.Repeat("a", 64), "0")
			b.Canonical = "different"
			switch kind {
			case "regional":
				a.Signature.Language = "en-US"
				b.Signature.Language = "en-GB"
			case "product":
				a.Product = true
			case "rotation":
				b.KeyID = "new-key"
			case "missing":
				a.Canonical = ""
				b.Canonical = ""
			}
			d.Add(a)
			d.Add(b)
			s, c, err := d.Analyze(t.Context())
			if err != nil || len(c) != 1 {
				t.Fatal(s, c, err)
			}
			if kind == "regional" && (c[0].Context != "regional_review" || c[0].PotentialOverlap) {
				t.Fatal(c)
			}
			if kind == "product" && (c[0].Context != "product_review" || c[0].PotentialOverlap) {
				t.Fatal(c)
			}
			if kind == "rotation" && (c[0].Canonical != "unverified" || s.CanonicalReviews != 0) {
				t.Fatal(s, c)
			}
			if kind == "missing" && c[0].Canonical != "missing" {
				t.Fatal(c)
			}
		})
	}
}

func TestIncompleteAndLegacySignaturesNotFabricated(t *testing.T) {
	d := NewDetector()
	d.Add(Document{TargetID: 1})
	p := testDocument(2, strings.Repeat("a", 64), "0")
	p.Signature.Complete = false
	d.Add(p)
	p = testDocument(3, strings.Repeat("b", 64), "0")
	p.Signature.Words = 20
	d.Add(p)
	s, c, err := d.Analyze(t.Context())
	if err != nil || s.Unverified != 2 || s.ShortContent != 1 || s.SignaturesComplete || len(c) != 0 {
		t.Fatal(s, c, err)
	}
}

func TestTenThousandDuplicatesUseLinearGrouping(t *testing.T) {
	d := NewDetector()
	for i := int64(1); i <= 10000; i++ {
		d.Add(testDocument(i, strings.Repeat("a", 64), "0"))
	}
	s, c, err := d.Analyze(t.Context())
	if err != nil || s.Analyzed != 10000 || s.Comparisons != 0 || s.ExactClusters != 1 || len(c) != 1 || c[0].Count != 10000 {
		t.Fatal(s, len(c), err)
	}
}

func TestCapacityAndComparisonBudgetsAreExplicit(t *testing.T) {
	d := NewDetector()
	for i := int64(1); i <= MaxPages+1; i++ {
		d.Add(testDocument(i, strings.Repeat("a", 64), "0"))
	}
	s, _, err := d.Analyze(t.Context())
	if err != nil || s.NearSearchComplete || s.CapacityExcluded != 1 || s.Eligible != MaxPages+1 {
		t.Fatal(s, err)
	}
	d = NewDetector()
	for i := int64(1); i <= 2000; i++ {
		d.Add(testDocument(i, fmt.Sprintf("%064x", i), fmt.Sprintf("%x", i)))
	}
	s, _, err = d.Analyze(t.Context())
	if err != nil || s.NearSearchComplete || s.Comparisons != MaxComparisons {
		t.Fatal(s, err)
	}
}

func TestCanceledAnalysisStops(t *testing.T) {
	d := NewDetector()
	d.Add(testDocument(1, strings.Repeat("a", 64), "0"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := d.Analyze(ctx); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestStreamingSignatureIsStableAcrossChunks(t *testing.T) {
	a, b := NewCollector(), NewCollector()
	a.Write([]byte("Straße café merchant protection"))
	for _, part := range []string{"Stra", "ße ca", "fé mer", "chant prote", "ction"} {
		b.Write([]byte(part))
	}
	sa, sb := a.Result("main", "", true), b.Result("main", "", true)
	if sa.SHA256 != sb.SHA256 || sa.SimHash != sb.SimHash || sa.Words != 4 {
		t.Fatal(sa, sb)
	}
}

func TestHeadingKeysHaveBoundedSize(t *testing.T) {
	if HeadingKey("  MERCHANT   protection ") != HeadingKey("merchant protection") {
		t.Fatal("heading normalization changed")
	}
	if len(HeadingKey(strings.Repeat("Україна ", 1000))) != 64 {
		t.Fatal("heading key retains unbounded metadata")
	}
	if HeadingKey(" ") != "" || HeadingKey("[Missing H1]") != "" {
		t.Fatal("missing heading was treated as an overlap")
	}
}

func TestExactSignaturePreservesCurrenciesAndPunctuation(t *testing.T) {
	collect := func(text string) *Signature {
		c := NewCollector()
		c.Write([]byte(text))
		return c.Result("main", "", true)
	}
	a, b := collect("Price $100. Merchant protection?"), collect("Price €100. Merchant protection.")
	if a.SHA256 == b.SHA256 {
		t.Fatal("meaningful symbols lost from exact signature")
	}
	if collect("Price   $100.").SHA256 != collect("price\n$100.").SHA256 {
		t.Fatal("whitespace normalization is unstable")
	}
}
