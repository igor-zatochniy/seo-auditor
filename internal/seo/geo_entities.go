package seo

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

const maxEntityNodes = 64
const maxEntitySampleBytes = 8 << 10

type schemaEntityRef struct {
	id     string
	record geo.EntityRecord
}

type schemaEntityNode struct {
	id                          string
	record                      geo.EntityRecord
	authors, publishers, brands []schemaEntityRef
}

func entityText(v any, max int) string {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max || strings.ContainsFunc(s, unicode.IsControl) {
		return ""
	}
	return s
}

func schemaTypes(v any) []string {
	values, ok := v.([]any)
	if !ok {
		values = []any{v}
	}
	result := []string{}
	for _, value := range values {
		t := strings.TrimPrefix(strings.TrimPrefix(entityText(value, 100), "https://schema.org/"), "http://schema.org/")
		if t != "" && len(result) < 8 {
			result = append(result, t)
		}
	}
	sort.Strings(result)
	return result
}

func schemaRecord(item map[string]any) geo.EntityRecord {
	r := geo.EntityRecord{Name: entityText(item["name"], 200), Types: schemaTypes(item["@type"])}
	values, ok := item["sameAs"].([]any)
	if !ok {
		values = []any{item["sameAs"]}
	}
	for _, value := range values {
		raw := entityText(value, 2048)
		u, err := url.Parse(raw)
		if err == nil && u.User == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && len(r.SameAs) < 4 {
			r.SameAs = append(r.SameAs, raw)
		}
	}
	return r
}

func schemaRefs(value any) []schemaEntityRef {
	items, ok := value.([]any)
	if !ok {
		items = []any{value}
	}
	refs := []schemaEntityRef{}
	for _, value := range items {
		if len(refs) == 8 {
			break
		}
		switch item := value.(type) {
		case map[string]any:
			refs = append(refs, schemaEntityRef{id: entityText(item["@id"], 2048), record: schemaRecord(item)})
		case string:
			if name := entityText(item, 200); name != "" {
				refs = append(refs, schemaEntityRef{record: geo.EntityRecord{Name: name}})
			}
		}
	}
	return refs
}

func (g *geoCollector) captureEntity(item map[string]any) {
	if values, ok := item["@type"].([]any); ok && len(values) > 8 {
		g.entityIncomplete = true
	}
	if name, ok := item["name"].(string); ok && name != "" && entityText(name, 200) == "" {
		g.entityIncomplete = true
	}
	if values, ok := item["sameAs"].([]any); ok && len(values) > 4 {
		g.entityIncomplete = true
	}
	r := schemaRecord(item)
	id := entityText(item["@id"], 2048)
	if len(r.Types) == 0 && id == "" {
		return
	}
	if len(g.entityNodes) == maxEntityNodes {
		g.entityIncomplete = true
		return
	}
	g.entityNodes = append(g.entityNodes, schemaEntityNode{id: id, record: r, authors: schemaRefs(item["author"]), publishers: schemaRefs(item["publisher"]), brands: schemaRefs(item["brand"])})
}

func (g *geoCollector) entitySample() *geo.EntitySample {
	s := &geo.EntitySample{Version: 1, Complete: !g.entityIncomplete && !g.signals.SchemaIncomplete}
	byID := map[string]geo.EntityRecord{}
	ambiguous := map[string]bool{}
	for _, node := range g.entityNodes {
		if node.id != "" && node.record.Name != "" {
			if previous, exists := byID[node.id]; exists && previous.Name != node.record.Name {
				ambiguous[node.id] = true
				s.Complete = false
			}
			byID[node.id] = node.record
		}
	}
	bytes := 0
	appendRecord := func(records *[]geo.EntityRecord, record geo.EntityRecord) {
		if record.Name == "" {
			return
		}
		for _, existing := range *records {
			if existing.Name == record.Name {
				return
			}
		}
		if len(*records) == 8 {
			s.Complete = false
			return
		}
		cost := len(record.Name) + 64
		for _, typ := range record.Types {
			cost += len(typ)
		}
		for _, value := range record.SameAs {
			cost += len(value)
		}
		if cost > maxEntitySampleBytes-bytes {
			s.Complete = false
			return
		}
		bytes += cost
		*records = append(*records, record)
	}
	resolve := func(refs []schemaEntityRef, records *[]geo.EntityRecord) {
		for _, ref := range refs {
			record := ref.record
			if record.Name == "" && ref.id != "" {
				if !ambiguous[ref.id] {
					record = byID[ref.id]
				}
				if record.Name == "" {
					s.Complete = false
				}
			}
			appendRecord(records, record)
		}
	}
	for _, node := range g.entityNodes {
		product := false
		for _, typ := range node.record.Types {
			switch typ {
			case "Organization", "Corporation", "LocalBusiness", "OnlineStore":
				appendRecord(&s.Organizations, node.record)
			case "Product":
				s.HasProduct, product = true, true
			}
		}
		resolve(node.authors, &s.Authors)
		resolve(node.publishers, &s.Publishers)
		if product {
			resolve(node.brands, &s.ProductBrands)
		}
	}
	return s
}
