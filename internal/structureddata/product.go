package structureddata

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ProductEvidence struct {
	Count            int  `json:"count"`
	WithPrice        int  `json:"with_price"`
	Complete         bool `json:"complete"`
	WithAvailability *int `json:"with_availability,omitempty"`
}
type productOffer struct {
	price        bool
	refs         []string
	complete     bool
	availability bool
}

func offerPrice(value any, depth int) productOffer {
	result := productOffer{complete: true}
	if depth > 4 {
		return productOffer{complete: false}
	}
	switch v := value.(type) {
	case map[string]any:
		if s, ok := v["availability"].(string); ok {
			s = strings.TrimPrefix(strings.TrimPrefix(s, "https://schema.org/"), "http://schema.org/")
			switch s {
			case "InStock", "OutOfStock", "PreOrder", "BackOrder", "PreSale", "LimitedAvailability", "OnlineOnly", "InStoreOnly", "SoldOut", "Discontinued":
				result.availability = true
			}
		}
		if id, ok := v["@id"].(string); ok && len(v) == 1 && len(id) <= 2048 {
			result.refs = []string{id}
			return result
		}
		for _, key := range []string{"price", "lowPrice"} {
			var n float64
			valid := false
			switch x := v[key].(type) {
			case float64:
				n, valid = x, true
			case string:
				if len(x) <= 32 {
					var e error
					n, e = strconv.ParseFloat(strings.TrimSpace(x), 64)
					valid = e == nil
				}
			}
			if valid && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
				result.price = true
			}
		}
		if spec, ok := v["priceSpecification"]; ok {
			p := offerPrice(spec, depth+1)
			result.price = result.price || p.price
			result.availability = result.availability || p.availability
			result.refs = append(result.refs, p.refs...)
			result.complete = result.complete && p.complete
		}
	case []any:
		if len(v) > 64 {
			v = v[:64]
			result.complete = false
		}
		for _, child := range v {
			p := offerPrice(child, depth+1)
			result.price = result.price || p.price
			result.availability = result.availability || p.availability
			result.complete = result.complete && p.complete
			result.refs = append(result.refs, p.refs...)
		}
	}
	return result
}
func (c *Collector) collectProduct(v map[string]any) {
	if len(c.products) >= 64 {
		c.Incomplete()
		return
	}
	c.products = append(c.products, offerPrice(v["offers"], 0))
}
func (c *Collector) productResult(pageURL string) *ProductEvidence {
	result := &ProductEvidence{Count: len(c.products), Complete: c.report.Complete && c.report.ValidBlocks == c.report.Blocks}
	availability := 0
	result.WithAvailability = &availability
	base, _ := url.Parse(pageURL)
	normalize := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil || base == nil {
			return raw
		}
		return base.ResolveReference(u).String()
	}
	prices := map[string]productOffer{}
	for id, price := range c.offerIDs {
		key := normalize(id)
		if previous, exists := prices[key]; exists {
			price.price = price.price || previous.price
			price.availability = price.availability || previous.availability
			price.complete = price.complete && previous.complete
			price.refs = append(append([]string{}, price.refs...), previous.refs...)
			if len(price.refs) > 128 {
				price.refs = price.refs[:128]
				price.complete = false
			}
		}
		prices[key] = price
	}
	for _, product := range c.products {
		price := product.price
		available := product.availability
		result.Complete = result.Complete && product.complete
		queue := append([]string{}, product.refs...)
		seen := map[string]bool{}
		for len(queue) > 0 && len(seen) < 128 {
			ref := normalize(queue[0])
			queue = queue[1:]
			if seen[ref] {
				continue
			}
			seen[ref] = true
			p, ok := prices[ref]
			if !ok {
				result.Complete = false
				continue
			}
			price = price || p.price
			available = available || p.availability
			result.Complete = result.Complete && p.complete
			queue = append(queue, p.refs...)
		}
		if len(queue) > 0 {
			result.Complete = false
		}
		if price {
			result.WithPrice++
		}
		if available {
			availability++
		}
	}
	return result
}

type ArticleEvidence struct {
	Count      int  `json:"count"`
	WithAuthor int  `json:"with_author"`
	WithDate   int  `json:"with_date"`
	Complete   bool `json:"complete"`
}

func (c *Collector) collectArticle(v map[string]any) {
	c.articles.Count++
	if hasValue(v["author"]) {
		c.articles.WithAuthor++
	}
	if s, ok := v["datePublished"].(string); ok {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if _, err := time.Parse(layout, s); err == nil {
				c.articles.WithDate++
				break
			}
		}
	}
}
