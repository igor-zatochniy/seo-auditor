package structureddata

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const Version = "jsonld-diagnostics-v1"
const MaxIssues = 64

type Issue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Block    int    `json:"block"`
	Path     string `json:"path"`
	Offset   int64  `json:"offset,omitempty"`
	Message  string `json:"message"`
}

type Report struct {
	Version     string   `json:"version"`
	Complete    bool     `json:"complete"`
	Blocks      int      `json:"blocks"`
	ValidBlocks int      `json:"valid_blocks"`
	Types       []string `json:"types"`
	Issues      []Issue  `json:"issues"`
}

type reference struct {
	id, path string
	block    int
}
type identity struct{ typ, name string }
type Collector struct {
	report    Report
	types     map[string]bool
	ids       map[string]identity
	refs      []reference
	nodes     int
	bytesRead int
	finalized bool
}

func NewCollector() *Collector {
	return &Collector{report: Report{Version: Version, Complete: true, Types: []string{}, Issues: []Issue{}}, types: map[string]bool{}, ids: map[string]identity{}}
}

func (c *Collector) Incomplete() { c.report.Complete = false }

func (c *Collector) AddBlock(raw []byte) {
	c.report.Blocks++
	c.bytesRead += len(raw)
	if c.report.Blocks > 64 || len(raw) > 64<<10 || c.bytesRead > 256<<10 {
		c.Incomplete()
		return
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		var syntax *json.SyntaxError
		issue := Issue{Code: "invalid_json", Severity: "error", Block: c.report.Blocks, Path: "$", Message: "Некоректний синтаксис JSON-LD"}
		if errors.As(err, &syntax) {
			issue.Offset = syntax.Offset
		}
		c.add(issue)
		return
	}
	if _, ok := value.(map[string]any); !ok {
		if _, ok = value.([]any); !ok {
			c.add(Issue{Code: "invalid_root", Severity: "error", Block: c.report.Blocks, Path: "$", Message: "Коренем JSON-LD має бути об'єкт або масив"})
			return
		}
	}
	c.report.ValidBlocks++
	c.visit(value, "$", 0)
}

func (c *Collector) add(issue Issue) {
	if len(c.report.Issues) >= MaxIssues {
		c.Incomplete()
		return
	}
	c.report.Issues = append(c.report.Issues, issue)
}

func schemaType(value any) string {
	s, _ := value.(string)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://schema.org/"), "http://schema.org/")
	if len(s) > 80 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return s
}

func hasValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func (c *Collector) visit(value any, path string, depth int) {
	c.nodes++
	if c.nodes > 2048 || depth > 16 {
		c.Incomplete()
		return
	}
	switch v := value.(type) {
	case map[string]any:
		typ := schemaType(v["@type"])
		types := []string{typ}
		if list, ok := v["@type"].([]any); ok {
			types = nil
			for _, t := range list {
				types = append(types, schemaType(t))
			}
		}
		for _, t := range types {
			if t != "" {
				if len(c.types) < 64 {
					c.types[t] = true
				} else {
					c.Incomplete()
				}
				c.checkProperties(v, t, path)
			}
		}
		if id, ok := v["@id"].(string); ok && len(id) <= 2048 {
			if len(v) == 1 {
				if len(c.refs) < 128 {
					c.refs = append(c.refs, reference{id, path, c.report.Blocks})
				} else {
					c.Incomplete()
				}
			} else if len(c.ids) < 128 {
				name, _ := v["name"].(string)
				if len(name) > 500 {
					name = name[:500]
				}
				if previous, exists := c.ids[id]; exists && (previous.typ != "" && typ != "" && previous.typ != typ || previous.name != "" && name != "" && previous.name != name) {
					c.add(Issue{Code: "conflicting_id", Severity: "warning", Block: c.report.Blocks, Path: path + ".@id", Message: "Один @id має суперечливі типи або назви сутності"})
				}
				c.ids[id] = identity{typ, name}
			} else {
				c.Incomplete()
			}
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if c.nodes >= 2048 {
				c.Incomplete()
				break
			}
			// Шлях не містить недовіреного тексту довільних ключів.
			childPath := fmt.Sprintf("%s.property[%d]", path, i)
			if knownProperty(key) {
				childPath = path + "." + key
			}
			if s, ok := v[key].(string); ok && (strings.Contains(s, "&amp;quot;") || strings.Contains(s, "&amp;lt;") || strings.Contains(s, "&amp;amp;")) {
				c.add(Issue{Code: "possible_overescaping", Severity: "warning", Block: c.report.Blocks, Path: childPath, Message: "Можливе повторне HTML-екранування значення; перевірте вихідну розмітку"})
			}
			c.visit(v[key], childPath, depth+1)
		}
	case []any:
		for i, child := range v {
			if c.nodes >= 2048 {
				c.Incomplete()
				break
			}
			c.visit(child, fmt.Sprintf("%s[%d]", path, i), depth+1)
		}
	}
}

func knownProperty(key string) bool {
	switch key {
	case "@graph", "@type", "@id", "@context", "name", "url", "headline", "author", "publisher", "image", "datePublished", "itemListElement", "position", "item", "offers", "review", "aggregateRating", "sameAs", "brand":
		return true
	}
	return false
}

func (c *Collector) checkProperties(v map[string]any, typ, path string) {
	missing := func(key, severity, message string) {
		if !hasValue(v[key]) {
			c.add(Issue{Code: "missing_" + key, Severity: severity, Block: c.report.Blocks, Path: path + "." + key, Message: message})
		}
	}
	switch typ {
	case "Product":
		missing("name", "warning", "Для Product snippets потрібна назва товару")
		if !hasValue(v["offers"]) && !hasValue(v["review"]) && !hasValue(v["aggregateRating"]) {
			c.add(Issue{Code: "product_snippet_properties", Severity: "warning", Block: c.report.Blocks, Path: path, Message: "Для Product snippets потрібне offers, review або aggregateRating; це не перевірка merchant listings"})
		}
	case "Article", "NewsArticle", "BlogPosting":
		for _, key := range []string{"headline", "author", "datePublished", "image"} {
			missing(key, "notice", "Для Article рекомендовано властивість "+key)
		}
	case "Organization":
		missing("name", "notice", "Для визначення організації рекомендовано name")
		missing("url", "notice", "Для визначення організації рекомендовано url")
	case "BreadcrumbList":
		if items, ok := v["itemListElement"].([]any); !ok || len(items) < 2 {
			c.add(Issue{Code: "breadcrumb_items", Severity: "warning", Block: c.report.Blocks, Path: path + ".itemListElement", Message: "Для breadcrumb rich result потрібні щонайменше два ListItem"})
		}
	case "ListItem":
		if position, ok := v["position"].(float64); !ok || position < 1 || position != float64(int64(position)) {
			c.add(Issue{Code: "invalid_position", Severity: "warning", Block: c.report.Blocks, Path: path + ".position", Message: "position має бути додатним цілим порядковим номером"})
		}
	}
}

func (c *Collector) Result(pageURL string) *Report {
	if c.finalized {
		result := c.report
		return &result
	}
	if c.report.Complete {
		base, err := url.Parse(pageURL)
		if err == nil {
			ids := make(map[string]bool, len(c.ids))
			for id := range c.ids {
				if u, e := url.Parse(id); e == nil {
					ids[base.ResolveReference(u).String()] = true
				}
			}
			for _, ref := range c.refs {
				u, err := url.Parse(ref.id)
				if err != nil {
					continue
				}
				u = base.ResolveReference(u)
				if u.Fragment != "" && strings.EqualFold(u.Host, base.Host) && u.Path == base.Path && u.RawQuery == base.RawQuery && !ids[u.String()] {
					c.add(Issue{Code: "unresolved_local_id", Severity: "warning", Block: ref.block, Path: ref.path + ".@id", Message: "Посилання @id на сутність цієї сторінки не знайдено у зібраних блоках"})
				}
			}
		}
	}
	result := c.report
	for typ := range c.types {
		result.Types = append(result.Types, typ)
	}
	sort.Strings(result.Types)
	c.report = result
	c.finalized = true
	return &result
}
