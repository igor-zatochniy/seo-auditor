package seo

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"github.com/igor-zatochniy/seo-auditor/internal/performance"
	"hash"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const comparisonItemLimit = 1000

// RenderComparison не містить виконуваного HTML або оригінальних мережевих відповідей.
type RenderComparison struct {
	Status      string              `json:"status"`
	Error       string              `json:"error,omitempty"`
	DurationMS  int64               `json:"duration_ms"`
	Requests    int                 `json:"requests"`
	Blocked     int                 `json:"blocked"`
	Bytes       int64               `json:"bytes"`
	Browser     string              `json:"browser,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	Raw         *RenderSnapshot     `json:"raw,omitempty"`
	Rendered    *RenderSnapshot     `json:"rendered,omitempty"`
	Delta       *RenderDelta        `json:"delta,omitempty"`
	Performance *performance.Result `json:"performance,omitempty"`
}

type RenderSnapshot struct {
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Canonical     string   `json:"canonical"`
	Robots        string   `json:"robots"`
	XRobotsTag    string   `json:"x_robots_tag"`
	H1            string   `json:"h1"`
	H1Count       int      `json:"h1_count"`
	Words         int      `json:"words"`
	TextSample    string   `json:"text_sample"`
	InternalLinks int      `json:"internal_links"`
	ExternalLinks int      `json:"external_links"`
	JSONLDCount   int      `json:"json_ld_count"`
	Hreflang      []string `json:"hreflang"`
	Complete      bool     `json:"complete"`
	evidence      *comparisonEvidence
}

type RenderDelta struct {
	Title                bool `json:"title"`
	Description          bool `json:"description"`
	Canonical            bool `json:"canonical"`
	Robots               bool `json:"robots"`
	H1                   bool `json:"h1"`
	H1OnlyAfterRendering bool `json:"h1_only_after_rendering"`
	Text                 bool `json:"text"`
	WordDelta            int  `json:"word_delta"`
	TextGrowthPercent    *int `json:"text_growth_percent,omitempty"`
	Links                bool `json:"links"`
	AddedInternalLinks   int  `json:"added_internal_links"`
	RemovedInternalLinks int  `json:"removed_internal_links"`
	JSONLD               bool `json:"json_ld"`
	JSONLDInjected       bool `json:"json_ld_injected"`
	Hreflang             bool `json:"hreflang"`
}

type comparisonEvidence struct {
	text       hash.Hash
	jsonLD     hash.Hash
	h1         hash.Hash
	sample     boundedTextCollector
	jsonActive bool
	jsonCount  int
	links      map[string]bool
	hreflang   []string
	complete   bool
}

func newComparisonEvidence() *comparisonEvidence {
	return &comparisonEvidence{text: sha256.New(), jsonLD: sha256.New(), h1: sha256.New(), sample: newBoundedTextCollector(2000), links: map[string]bool{}, complete: true}
}

func ParsePageForComparison(resp *http.Response, targetURL string, maxBodyBytes, maxTokenBytes int64, collectLinks bool) (Data, error) {
	return parsePage(resp, targetURL, maxBodyBytes, maxTokenBytes, collectLinks, true)
}

func (p *pageParser) comparisonStart(name []byte, a tagAttributes) {
	e := p.comparison
	if e == nil {
		return
	}
	if bytes.Equal(name, []byte("script")) {
		e.jsonActive = p.shadowRootDepth == 0 && bytes.EqualFold(bytes.TrimSpace(a.typeValue), []byte("application/ld+json"))
		if e.jsonActive {
			e.jsonCount++
			_, _ = e.jsonLD.Write([]byte{0})
		}
	}
	if bytes.Equal(name, []byte("a")) && a.hasHref {
		if u := p.comparisonURL(a.href); u != nil {
			if len(e.links) < comparisonItemLimit {
				e.links[u.String()] = sameNormalizedHost(u, p.targetURL)
			} else {
				e.complete = false
			}
		}
	}
	if bytes.Equal(name, []byte("link")) && p.inDocumentHead() && len(a.hreflang) > 0 && hasTokenFold(a.rel, []byte("alternate")) {
		if u := p.comparisonURL(a.href); u != nil {
			if len(e.hreflang) < 32 && len(a.hreflang) <= 64 {
				e.hreflang = append(e.hreflang, strings.ToLower(string(a.hreflang))+" "+u.String())
			} else {
				e.complete = false
			}
		}
	}
	if bytes.Equal(name, []byte("h1")) {
		_, _ = e.h1.Write([]byte{0})
	}
}

func (p *pageParser) comparisonURL(raw []byte) *url.URL {
	if len(raw) > 2048 || p.documentBaseURL == nil {
		if p.comparison != nil {
			p.comparison.complete = false
		}
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || len(raw) == 0 || raw[0] == '#' {
		return nil
	}
	u = p.documentBaseURL.ResolveReference(u)
	if u.Scheme != "http" && u.Scheme != "https" || u.User != nil {
		return nil
	}
	u.Fragment = ""
	u.Host = normalizedAuthority(u)
	if len(u.String()) > 2048 {
		p.comparison.complete = false
		return nil
	}
	return u
}

func Snapshot(data Data) *RenderSnapshot {
	e := data.comparison
	s := &RenderSnapshot{Title: data.Title, Description: data.Description, Canonical: data.CanonicalURL, Robots: data.MetaRobots, XRobotsTag: data.XRobotsTag, H1: data.H1, H1Count: data.H1Count, Words: data.WordCount, InternalLinks: data.InternalLinksCount, ExternalLinks: data.ExternalLinksCount, Hreflang: []string{}, evidence: e}
	if e != nil {
		s.TextSample, _, _ = e.sample.Result()
		s.JSONLDCount = e.jsonCount
		s.Hreflang = append(s.Hreflang, e.hreflang...)
		slices.Sort(s.Hreflang)
		s.Complete = e.complete && data.HTMLSizeComplete && !data.TitleTruncated && !data.H1Truncated && !data.CanonicalURLTruncated && !data.MetaRobotsTruncated
		if data.DescriptionCharCount != nil && *data.DescriptionCharCount > StorageDescriptionMaxRunes {
			s.Complete = false
		}
	}
	return s
}

func Compare(raw, rendered *RenderSnapshot) *RenderDelta {
	d := &RenderDelta{Title: raw.Title != rendered.Title, Description: raw.Description != rendered.Description, Canonical: raw.Canonical != rendered.Canonical, Robots: raw.Robots != rendered.Robots, H1: raw.H1 != rendered.H1 || raw.H1Count != rendered.H1Count, H1OnlyAfterRendering: raw.H1Count == 0 && rendered.H1Count > 0, WordDelta: rendered.Words - raw.Words, Hreflang: !slices.Equal(raw.Hreflang, rendered.Hreflang), JSONLDInjected: raw.JSONLDCount == 0 && rendered.JSONLDCount > 0}
	a, b := raw.evidence, rendered.evidence
	d.Links = raw.InternalLinks != rendered.InternalLinks || raw.ExternalLinks != rendered.ExternalLinks
	if a == nil || b == nil {
		return d
	}
	d.H1 = d.H1 || !bytes.Equal(a.h1.Sum(nil), b.h1.Sum(nil))
	d.Text = !bytes.Equal(a.text.Sum(nil), b.text.Sum(nil))
	d.JSONLD = raw.JSONLDCount != rendered.JSONLDCount || !bytes.Equal(a.jsonLD.Sum(nil), b.jsonLD.Sum(nil))
	for link, internal := range b.links {
		if _, ok := a.links[link]; !ok {
			d.Links = true
			if internal {
				d.AddedInternalLinks++
			}
		}
	}
	for link, internal := range a.links {
		if _, ok := b.links[link]; !ok {
			d.Links = true
			if internal {
				d.RemovedInternalLinks++
			}
		}
	}
	if raw.Complete && rendered.Complete && rendered.Words > 0 {
		v := max(0, 100*(rendered.Words-raw.Words)/rendered.Words)
		d.TextGrowthPercent = &v
	}
	return d
}

func (r *RenderComparison) Sanitize(text, safeURL func(string) string) {
	r.Error = text(r.Error)
	if r.Performance != nil {
		r.Performance.Error = text(r.Performance.Error)
	}
	for _, s := range []*RenderSnapshot{r.Raw, r.Rendered} {
		if s == nil {
			continue
		}
		s.Title = text(s.Title)
		s.Description = text(s.Description)
		s.Canonical = safeURL(s.Canonical)
		s.Robots = text(s.Robots)
		s.XRobotsTag = text(s.XRobotsTag)
		s.H1 = text(s.H1)
		s.TextSample = text(s.TextSample)
		for i, v := range s.Hreflang {
			s.Hreflang[i] = text(v)
		}
	}
	if len(r.JSON()) > 200000 {
		for _, s := range []*RenderSnapshot{r.Raw, r.Rendered} {
			if s != nil {
				s.Hreflang = nil
				s.Complete = false
			}
		}
		if r.Status == "completed" {
			r.Status = "partial"
		}
		r.Warnings = append(r.Warnings, "Деталі hreflang не збережено через ліміт розміру звіту.")
	}
}

func (r *RenderComparison) JSON() []byte {
	if r == nil {
		return nil
	}
	b, _ := json.Marshal(r)
	return b
}
