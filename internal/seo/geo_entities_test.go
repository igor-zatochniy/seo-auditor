package seo

import (
	"fmt"
	"strings"
	"testing"
)

func TestGEOEntityGraphReferencesAcrossJSONLDBlocks(t *testing.T) {
	body := `<head><title>Original title</title><script type="application/ld+json">{"@graph":[{"@type":"Article","publisher":{"@id":"#org"},"author":{"@id":"#person"}},{"@type":"Product","brand":{"@id":"#org"}}]}</script><script type="application/ld+json">[{"@id":"#org","@type":"Organization","name":"Merchanto","sameAs":["https://example.com/company?token=secret"]},{"@id":"#person","@type":"Person","name":"Editor"}]</script></head><body><main><h1>Original H1</h1><p>Merchanto protects payments</p></main></body>`
	data := parseGEOTest(t, body)
	s := data.GEO.Entities
	if s == nil || !s.Complete || len(s.Organizations) != 1 || len(s.Authors) != 1 || len(s.Publishers) != 1 || len(s.ProductBrands) != 1 || s.Publishers[0].Name != "Merchanto" || s.Authors[0].Name != "Editor" {
		t.Fatalf("Граф не розв'язано: %+v", s)
	}
	baseline := parseGEOTest(t, `<head><title>Original title</title></head><body><main><h1>Original H1</h1><p>Merchanto protects payments</p></main></body>`)
	if data.Title != baseline.Title || data.H1 != baseline.H1 || data.WordCount != baseline.WordCount {
		t.Fatalf("Пошкоджено звичайні SEO-метрики: %+v", data)
	}
}

func TestGEOEntityParsingIsBoundedAndIgnoresInertTemplates(t *testing.T) {
	items := []string{}
	for i := range 100 {
		items = append(items, fmt.Sprintf(`{"@type":"Organization","name":"Brand %d"}`, i))
	}
	data := parseGEOTest(t, `<head><script type="application/ld+json">[`+strings.Join(items, ",")+`]</script></head><body><h1>H1</h1></body>`)
	if data.GEO.Entities.Complete || len(data.GEO.Entities.Organizations) > 8 {
		t.Fatal("Ліміти сутностей не діють")
	}
	data = parseGEOTest(t, `<body><template><script type="application/ld+json">{"@type":"Organization","name":"Fake"}</script></template><main>Real</main></body>`)
	if len(data.GEO.Entities.Organizations) != 0 {
		t.Fatal("Inert template вплинув на сутності")
	}
	data = parseGEOTest(t, `<head><script type="application/ld+json">{"@type":"Article","publisher":{"@id":"#missing"}}</script></head>`)
	if data.GEO.Entities.Complete {
		t.Fatal("Невідомий @id видано за відсутній publisher")
	}
	collector := newGEOCollector()
	for i := range 32 {
		collector.captureEntity(map[string]any{"@type": "Organization", "name": fmt.Sprintf("Brand %d", i), "sameAs": []any{"https://example.com/" + strings.Repeat("a", 1800), "https://example.com/" + strings.Repeat("b", 1800)}})
	}
	sample := collector.entitySample()
	bytes := 0
	for _, record := range sample.Organizations {
		bytes += len(record.Name) + 64
		for _, typ := range record.Types {
			bytes += len(typ)
		}
		for _, value := range record.SameAs {
			bytes += len(value)
		}
	}
	if sample.Complete || bytes > maxEntitySampleBytes {
		t.Fatal("Не обмежено сумарний розмір вибірки сутностей")
	}
}
