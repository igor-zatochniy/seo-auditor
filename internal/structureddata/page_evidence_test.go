package structureddata

import "testing"

func TestPageEvidenceAvailabilityAndArticle(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"@graph":[{"@type":"Product","offers":{"@id":"#offer"}},{"@type":"Offer","@id":"#offer","price":10,"availability":"https://schema.org/InStock"},{"@type":"Article","author":{"name":"Writer"},"datePublished":"2026-10-18"}]}`))
	r := c.Result("https://example.com/p")
	if r.Products == nil || r.Products.WithAvailability == nil || *r.Products.WithAvailability != 1 || r.Products.WithPrice != 1 || !r.Products.Complete {
		t.Fatalf("%+v", r.Products)
	}
	if r.Articles == nil || r.Articles.Count != 1 || r.Articles.WithDate != 1 || r.Articles.WithAuthor != 1 || !r.Articles.Complete {
		t.Fatalf("%+v", r.Articles)
	}
	c = NewCollector()
	c.AddBlock([]byte(`{"@type":"Article","datePublished":"not-a-date"}`))
	c.Incomplete()
	r = c.Result("https://example.com/")
	if r.Articles.WithDate != 0 || r.Articles.Complete {
		t.Fatalf("%+v", r.Articles)
	}
}

func TestProductEvidenceMergesEquivalentOfferIDs(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"@graph":[{"@type":"Product","offers":{"@id":"#offer"}},{"@type":"Offer","@id":"#offer","price":25},{"@type":"Offer","@id":"https://example.com/product#offer","availability":"https://schema.org/InStock"}]}`))
	r := c.Result("https://example.com/product")
	if r.Products == nil || r.Products.WithPrice != 1 || r.Products.WithAvailability == nil || *r.Products.WithAvailability != 1 || !r.Products.Complete {
		t.Fatalf("equivalent offer fragments lost: %+v", r.Products)
	}
}
