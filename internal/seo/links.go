package seo

import (
	"net/url"
	"strings"
)

const MaxDiscoveredLinks = 256
const MaxDiscoveredURLBytes = 2048

// Request URLs are transient, excluded from JSON, and must be redacted at storage boundaries.
type DiscoveredLink struct {
	URL      string
	Anchor   string
	Nofollow bool
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
	if len(p.data.DiscoveredLinks) >= MaxDiscoveredLinks || len(a.href) > MaxDiscoveredURLBytes {
		p.data.LinksTruncated = true
		return
	}
	href := strings.TrimSpace(string(a.href))
	if href == "" || strings.HasPrefix(href, "#") {
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
	p.data.DiscoveredLinks = append(p.data.DiscoveredLinks, DiscoveredLink{URL: c.href, Anchor: anchor, Nofollow: c.nofollow})
	c.active = false
}

func (p *pageParser) resolveLinks() {
	if p.documentBaseURL == nil {
		p.data.DiscoveredLinks = nil
		return
	}
	links := p.data.DiscoveredLinks[:0]
	for _, link := range p.data.DiscoveredLinks {
		parsed, err := url.Parse(link.URL)
		if err != nil {
			continue
		}
		resolved := p.documentBaseURL.ResolveReference(parsed)
		resolved.Fragment, resolved.RawFragment = "", ""
		if resolved.User != nil || (resolved.Scheme != "http" && resolved.Scheme != "https") {
			continue
		}
		link.URL = resolved.String()
		if len(link.URL) > MaxDiscoveredURLBytes {
			p.data.LinksTruncated = true
			continue
		}
		links = append(links, link)
	}
	p.data.DiscoveredLinks = links
}
