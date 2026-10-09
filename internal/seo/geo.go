package seo

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

type geoCollector struct {
	regions               [3]geoRegion
	state                 geoContentState
	stack                 []geoFrame
	mainSeen, articleSeen bool
	jsonLD                bool
	jsonText              []byte
	jsonBytes             int
	types                 map[string]bool
	signals               geo.Signals
	entityNodes           []schemaEntityNode
	entityIncomplete      bool
}

func newGEOCollector() geoCollector {
	g := geoCollector{types: make(map[string]bool)}
	for i := range g.regions {
		g.regions[i] = geoRegion{content: newBoundedTextCollector(2000), headings: newBoundedTextCollector(800), paragraph: newBoundedTextCollector(600)}
	}
	g.signals.Search = geo.SearchControls{GooglebotRules: geo.Unknown, OAISearchBotRules: geo.Unknown, GoogleIndexing: geo.Allowed, GoogleSnippet: geo.Allowed}
	return g
}

func (p *pageParser) geoStart(name []byte, attrs tagAttributes) {
	g := &p.geo
	if p.shadowRootDepth == 0 && bytes.Equal(name, []byte("meta")) && attrs.hasName && attrs.hasContent &&
		(bytes.EqualFold(bytes.TrimSpace(attrs.name), []byte("robots")) || bytes.EqualFold(bytes.TrimSpace(attrs.name), []byte("googlebot"))) {
		g.readGoogleDirectives(string(attrs.content))
	}
	if attrs.dataNoSnippet && (bytes.Equal(name, []byte("div")) || bytes.Equal(name, []byte("span")) || bytes.Equal(name, []byte("section"))) {
		g.signals.Search.DataNoSnippet = true
	}
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
	g.startContent(name, attrs)
}

func (g *geoCollector) text(text []byte) {
	if g.state.excluded || g.signals.ContentIncomplete {
		return
	}
	for i := range g.regions {
		if !g.regionActive(i) {
			continue
		}
		r := &g.regions[i]
		r.content.Write(text)
		r.content.Write([]byte(" "))
		if g.state.heading {
			r.headings.Write(text)
			r.headingText.write(text)
		}
		if r.blockActive {
			r.block.write(text)
		}
		if r.inParagraph {
			r.paragraph.Write(text)
		}
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
	if string(name) == "script" {
		if g.jsonLD {
			g.parseSchema()
		}
		g.jsonLD = false
		g.jsonText = nil
	}
	g.endContent(name)
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
			g.captureEntity(item)
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
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if nodes >= 2048 {
					g.signals.SchemaIncomplete = true
					break
				}
				visit(item[key], depth+1)
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
	s.Version, s.Complete = 3, true
	index := 0
	s.ContentSource = "body"
	if g.mainSeen && g.regions[1].content.RuneCount() > 0 {
		index, s.ContentSource = 1, "main"
	} else if g.articleSeen && g.regions[2].content.RuneCount() > 0 {
		index, s.ContentSource = 2, "article"
	}
	r := &g.regions[index]
	r.finishBlock()
	s.Blocks = &geo.BlockSample{Count: r.blockCount, Complete: !s.ContentIncomplete && r.blockCount <= maxGEOBlocks, Items: r.blocks}
	s.HasList, s.HasTable = r.hasList, r.hasTable
	s.HasAuthor = s.HasAuthor || r.hasAuthor
	var clipped bool
	s.Excerpt, clipped, _ = r.content.Result()
	s.SampleTruncated = clipped
	s.Headings, clipped, _ = r.headings.Result()
	s.SampleTruncated = s.SampleTruncated || clipped
	s.FirstParagraph, _, _ = r.paragraph.Result()
	s.SchemaTypes = make([]string, 0, len(g.types))
	for typ := range g.types {
		s.SchemaTypes = append(s.SchemaTypes, typ)
	}
	sort.Strings(s.SchemaTypes)
	s.Entities = g.entitySample()
	return &s
}
