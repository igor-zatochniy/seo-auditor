package semantic

import "testing"

func TestSixLanguageConceptParity(t *testing.T) {
	for _, query := range []string{"payment protection", "захист платежів", "защита платежей", "ochrona płatności", "protección pagos", "protecție plăți"} {
		units, limited := Extract(query)
		keys := map[string]bool{}
		for _, u := range units {
			keys[u.Key] = true
		}
		if limited || len(units) != 2 || !keys["payment"] || !keys["protection"] {
			t.Fatalf("%q: %+v limited=%v", query, units, limited)
		}
	}
}

func TestConceptBoundariesAndUnknownNames(t *testing.T) {
	u, _ := Extract("Stripe programming software")
	if len(u) != 3 || u[0].Known || u[0].Key != "stripe" || u[1].Known || !u[2].Known {
		t.Fatal(u)
	}
	u, _ = Extract("програмне забезпечення платежів")
	if len(u) != 2 || u[0].Key != "software" || u[1].Key != "payment" {
		t.Fatal(u)
	}
	u, _ = Extract("refund chargeback")
	if len(u) != 2 || u[0].Key == u[1].Key {
		t.Fatal("Повернення коштів змішано з чарджбеком", u)
	}
}
