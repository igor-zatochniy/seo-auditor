package geo

import "testing"

func TestEntityAssessmentIsIndependentAndConservative(t *testing.T) {
	s := &Signals{Complete: true, Excerpt: "Merchanto protects payments", Entities: &EntitySample{Version: 1, Complete: true, HasProduct: true,
		Organizations: []EntityRecord{{Name: "MERCHANTO", SameAs: []string{"https://example.com/brand"}}},
		Authors:       []EntityRecord{{Name: "Editor"}}, Publishers: []EntityRecord{{Name: "Merchanto"}}, ProductBrands: []EntityRecord{{Name: "Merchanto"}}}}
	r := EvaluateEntities("Merchanto", s)
	for _, c := range r.Checks {
		if c.Status != "found" {
			t.Fatalf("Немає сигналу: %+v", c)
		}
	}
	s.Entities.Publishers[0].Name = "Different publisher"
	if got := EvaluateEntities("Merchanto", s).Checks[3].Status; got != "mismatch" {
		t.Fatal(got)
	}
	s.SampleTruncated = true
	s.Excerpt = "No matching name in this sample"
	if got := EvaluateEntities("Merchanto", s).Checks[0].Status; got != Unknown {
		t.Fatal("Обрізану вибірку видано за відсутність бренду")
	}
	s.Entities.Complete = false
	s.Entities.Authors = nil
	if got := EvaluateEntities("Merchanto", s).Checks[4].Status; got != Unknown {
		t.Fatal("Неповні дані видано за відсутність автора")
	}
	for _, c := range EvaluateEntities("Merchanto", &Signals{Complete: true}).Checks {
		if c.Status != Unknown {
			t.Fatal("Старі сигнали переоцінено")
		}
	}
	if containsBrand("MerchantoPlus", "Merchanto") {
		t.Fatal("Збіг усередині іншої назви")
	}
}
