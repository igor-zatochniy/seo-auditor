package seo

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

type geoCollector struct {
	content, headings, paragraph          boundedTextCollector
	paragraphSeen, inParagraph, inHeading bool
	jsonLD                                bool
	jsonText                              []byte
	jsonBytes                             int
	types                                 map[string]bool
	signals                               geo.Signals
}

func newGEOCollector() geoCollector {
	return geoCollector{content: newBoundedTextCollector(2000), headings: newBoundedTextCollector(800),
		paragraph: newBoundedTextCollector(600), types: make(map[string]bool)}
}

func (p *pageParser) geoStart(name []byte, attrs tagAttributes) {
	g := &p.geo
	if p.inDocumentHead() && bytes.Equal(name, []byte("meta")) && attrs.hasName &&
		bytes.EqualFold(attrs.name, []byte("author")) && len(bytes.TrimSpace(attrs.content)) > 0 {
		g.signals.HasAuthor = true
	}
	if bytes.Equal(name, []byte("script")) && p.shadowRootDepth == 0 &&
		bytes.EqualFold(bytes.TrimSpace(attrs.typeValue), []byte("application/ld+json")) {
		g.jsonLD = true
		g.jsonText = nil
	}
	if p.documentPhase != documentPhaseBody || p.ignoredTextDepth > 0 {
		return
	}
	switch string(name) {
	case "p":
		if !g.paragraphSeen {
			g.paragraphSeen = true
			g.inParagraph = true
		}
	case "h1", "h2", "h3", "h4", "h5", "h6":
		g.inHeading = true
		g.headings.Write([]byte(" "))
	case "table":
		g.signals.HasTable = true
	case "ul", "ol":
		g.signals.HasList = true
	case "a":
		if attrs.hasRel && hasTokenFold(attrs.rel, []byte("author")) {
			g.signals.HasAuthor = true
		}
	}
}

func (g *geoCollector) text(text []byte) {
	g.content.Write(text)
	g.content.Write([]byte(" "))
	if g.inHeading {
		g.headings.Write(text)
	}
	if g.inParagraph {
		g.paragraph.Write(text)
	}
}

func (g *geoCollector) scriptText(text []byte) {
	if !g.jsonLD {
		return
	}
	// Ліміти діють на один блок та сумарно на всі JSON-LD блоки сторінки.
	if len(text) > (64<<10)-len(g.jsonText) || len(text) > (256<<10)-g.jsonBytes {
		g.signals.SchemaIncomplete = true
		g.jsonLD = false
		g.jsonText = nil
		return
	}
	g.jsonBytes += len(text)
	g.jsonText = append(g.jsonText, text...)
}

func (g *geoCollector) end(name []byte) {
	switch string(name) {
	case "p":
		g.inParagraph = false
	case "h1", "h2", "h3", "h4", "h5", "h6":
		g.inHeading = false
	case "script":
		if g.jsonLD {
			g.parseSchema()
		}
		g.jsonLD = false
		g.jsonText = nil
	}
}

func (g *geoCollector) parseSchema() {
	var value any
	if json.Unmarshal(g.jsonText, &value) != nil {
		g.signals.SchemaIncomplete = true
		return
	}
	nodes := 0
	var visit func(any, int)
	visit = func(v any, depth int) {
		nodes++
		if nodes > 2048 || depth > 16 {
			g.signals.SchemaIncomplete = true
			return
		}
		switch item := v.(type) {
		case map[string]any:
			add := func(v any) {
				s, _ := v.(string)
				s = strings.TrimPrefix(strings.TrimPrefix(s, "https://schema.org/"), "http://schema.org/")
				switch s {
				case "Organization", "Product", "FAQPage", "Article", "Person", "HowTo", "WebPage", "Review", "Service":
					g.types[s] = true
				}
			}
			if types, ok := item["@type"].([]any); ok {
				for _, typ := range types {
					add(typ)
				}
			} else {
				add(item["@type"])
			}
			for _, child := range item {
				if nodes >= 2048 {
					g.signals.SchemaIncomplete = true
					break
				}
				visit(child, depth+1)
			}
		case []any:
			for _, child := range item {
				if nodes >= 2048 {
					g.signals.SchemaIncomplete = true
					break
				}
				visit(child, depth+1)
			}
		}
	}
	visit(value, 0)
}

func (g *geoCollector) result() *geo.Signals {
	s := g.signals
	s.Version, s.Complete = 1, true
	var clipped bool
	s.Excerpt, clipped, _ = g.content.Result()
	s.SampleTruncated = clipped
	s.Headings, clipped, _ = g.headings.Result()
	s.SampleTruncated = s.SampleTruncated || clipped
	s.FirstParagraph, _, _ = g.paragraph.Result()
	s.SchemaTypes = make([]string, 0, len(g.types))
	for typ := range g.types {
		s.SchemaTypes = append(s.SchemaTypes, typ)
	}
	sort.Strings(s.SchemaTypes)
	return &s
}
