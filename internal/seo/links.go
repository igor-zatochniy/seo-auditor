package seo

import (
	"net/url"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
)

const MaxDiscoveredLinks = 256
const MaxDiscoveredURLBytes = 2048

// Request URLs are transient, excluded from JSON, and must be redacted at storage boundaries.
type DiscoveredLink struct {
	URL      string
	Anchor   string
	Nofollow bool
	Source   string
}

type linkCollector struct {
	active   bool
	href     string
	nofollow bool
	anchor   boundedTextCollector
}

func (p *pageParser) startLink(a tagAttributes) {
	if !p.collectLinks {
		return
	}
	if len(a.href) > MaxDiscoveredURLBytes {
		p.data.LinksTruncated = true
		return
	}
	href := strings.TrimSpace(string(a.href))
	if href == "" || strings.HasPrefix(href, "#") {
		return
	}
	parsed, err := url.Parse(href)
	if err != nil || p.documentBaseURL == nil {
		return
	}
	resolved := p.documentBaseURL.ResolveReference(parsed)
	resolved.Scheme = strings.ToLower(resolved.Scheme)
	if resolved.User != nil || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return
	}
	authority, err := crawler.NormalizeAuthority(resolved)
	if err != nil {
		return
	}
	resolved.Host = authority
	resolved.Fragment, resolved.RawFragment = "", ""
	if resolved.Path == "" {
		resolved.Path = "/"
	}
	href = resolved.String()
	if len(href) > MaxDiscoveredURLBytes {
		p.data.LinksTruncated = true
		return
	}
	if p.discoveredURLs == nil {
		p.discoveredURLs = make(map[string]int)
	}
	if _, duplicate := p.discoveredURLs[href]; !duplicate && len(p.data.DiscoveredLinks) >= MaxDiscoveredLinks {
		p.data.LinksTruncated = true
		return
	}
	p.linkCollector = linkCollector{active: true, href: href,
		nofollow: hasTokenFold(a.rel, []byte("nofollow")), anchor: newBoundedTextCollector(256)}
}

func (p *pageParser) finishLink() {
	c := &p.linkCollector
	if !c.active {
		return
	}
	anchor, _, _ := c.anchor.Result()
	if i, duplicate := p.discoveredURLs[c.href]; duplicate {
		link := &p.data.DiscoveredLinks[i]
		link.Nofollow = link.Nofollow && c.nofollow
		if link.Anchor == "" {
			link.Anchor = anchor
		}
	} else {
		p.discoveredURLs[c.href] = len(p.data.DiscoveredLinks)
		p.data.DiscoveredLinks = append(p.data.DiscoveredLinks, DiscoveredLink{URL: c.href, Anchor: anchor, Nofollow: c.nofollow})
	}
	c.active = false
}
