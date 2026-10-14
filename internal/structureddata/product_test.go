package structureddata

import "testing"

func TestProductPricesResolveReferencesWithoutRetainingValues(t *testing.T) {
	cases := []struct {
		raw           string
		count, priced int
		complete      bool
	}{
		{`{"@type":"Product","offers":{"@type":"Offer","price":"0"}}`, 1, 1, true},
		{`{"@type":"Product","offers":[{"price":23}]}`, 1, 1, true},
		{`{"@type":"Product","offers":{"priceSpecification":{"@type":"UnitPriceSpecification","price":99}}}`, 1, 1, true},
		{`{"@graph":[{"@type":"Product","offers":{"@id":"#offer"}},{"@id":"https://example.com/p#offer","@type":"Offer","price":20}]}`, 1, 1, true},
		{`{"@type":"Product","offers":{"@id":"#missing"}}`, 1, 0, false},
		{`{"@type":"Product","offers":{"price":"NaN"}}`, 1, 0, true},
		{`{"@type":"Product","offers":{"price":-1}}`, 1, 0, true},
	}
	for _, v := range cases {
		c := NewCollector()
		c.AddBlock([]byte(v.raw))
		r := c.Result("https://example.com/p")
		if r.Products.Count != v.count || r.Products.WithPrice != v.priced || r.Products.Complete != v.complete {
			t.Fatalf("%s: %+v", v.raw, r.Products)
		}
	}
	c := NewCollector()
	c.AddBlock([]byte(`not json`))
	if c.Result("https://example.com/").Products.Complete {
		t.Fatal("invalid JSON claimed complete")
	}
}
