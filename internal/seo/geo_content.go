package seo

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

type geoRegion struct {
	content, headings, paragraph boundedTextCollector
	paragraphSeen, inParagraph   bool
	hasList, hasTable, hasAuthor bool
	block, headingText           blockText
	blockActive                  bool
	blockHeading, latestHeading  string
	blockCount                   int
	blocks                       []geo.TextBlock
}

type geoContentState struct{ excluded, main, article, heading bool }
type geoFrame struct {
	name     string
	previous geoContentState
}

func (g *geoCollector) regionActive(i int) bool {
	return i == 0 || i == 1 && g.state.main || i == 2 && g.state.article
}

// Три обмежені вибірки дозволяють вибрати main навіть після довгого меню.
// Стек потрібний лише GEO; лічильники звичайного SEO його не використовують.
func (g *geoCollector) startContent(name []byte, attrs tagAttributes) {
	if g.signals.ContentIncomplete {
		return
	}
	tag := string(name)
	previous := g.state
	if !geoVoidElement(tag) {
		if len(g.stack) >= 128 || len(name) > 64 {
			g.signals.ContentIncomplete = true
			return
		}
		g.stack = append(g.stack, geoFrame{tag, previous})
	}
	role := strings.ToLower(strings.TrimSpace(string(attrs.role)))
	if tag == "nav" || tag == "aside" || tag == "dialog" || role == "navigation" || role == "complementary" || role == "dialog" ||
		attrs.hidden || bytes.EqualFold(bytes.TrimSpace(attrs.ariaHidden), []byte("true")) ||
		(!g.state.main && !g.state.article && (tag == "header" || tag == "footer" || role == "banner" || role == "contentinfo")) {
		g.state.excluded = true
	}
	if !g.state.excluded {
		if tag == "main" || role == "main" {
			g.state.main, g.mainSeen = true, true
		}
		if tag == "article" {
			g.state.article, g.articleSeen = true, true
		}
		if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
			g.state.heading = true
		}
		for i := range g.regions {
			if !g.regionActive(i) {
				continue
			}
			r := &g.regions[i]
			r.blockStart(tag)
			switch tag {
			case "p":
				if !r.paragraphSeen {
					r.paragraphSeen, r.inParagraph = true, true
				}
			case "ul", "ol":
				r.hasList = true
			case "table":
				r.hasTable = true
			case "a":
				r.hasAuthor = r.hasAuthor || hasTokenFold(attrs.rel, []byte("author"))
			}
			if g.state.heading && !previous.heading {
				r.headings.Write([]byte(" "))
			}
		}
	}
	if geoVoidElement(tag) {
		g.state = previous
	}
}

func geoVoidElement(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	}
	return false
}

func (g *geoCollector) endContent(name []byte) {
	if g.signals.ContentIncomplete {
		return
	}
	for i := len(g.stack) - 1; i >= 0; i-- {
		if g.stack[i].name != string(name) {
			continue
		}
		for _, frame := range g.stack[i:] {
			if !g.state.excluded {
				for j := range g.regions {
					if g.regionActive(j) {
						g.regions[j].blockEnd(frame.name)
					}
				}
			}
			if frame.name == "p" {
				for j := range g.regions {
					g.regions[j].inParagraph = false
				}
			}
		}
		g.state = g.stack[i].previous
		g.stack = g.stack[:i]
		return
	}
}

func (g *geoCollector) readSearchHeaders(headers http.Header) {
	for _, raw := range headers.Values("X-Robots-Tag") {
		scope, rules := splitXRobotsTagScope(raw)
		if scope == "" || scope == "googlebot" {
			g.readGoogleDirectives(rules)
		}
	}
}

func (g *geoCollector) readGoogleDirectives(raw string) {
	s := &g.signals.Search
	for _, directive := range splitRobotsDirectives(raw) {
		switch robotsDirectiveName(directive) {
		case "noindex", "none":
			s.GoogleIndexing = geo.Blocked
		case "nosnippet":
			s.GoogleSnippet = geo.Blocked
		case "max-snippet":
			_, value, ok := strings.Cut(directive, ":")
			limit, err := strconv.Atoi(strings.TrimSpace(value))
			if !ok || err != nil || limit < -1 {
				continue
			}
			if s.MaxSnippet == nil || *s.MaxSnippet == -1 || limit >= 0 && limit < *s.MaxSnippet {
				s.MaxSnippet = &limit
			}
			if limit == 0 {
				s.GoogleSnippet = geo.Blocked
			}
		}
	}
}
