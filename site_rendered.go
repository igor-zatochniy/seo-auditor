package main

import "github.com/igor-zatochniy/seo-auditor/internal/seo"

// Зберігаємо HTML-посилання; однакові DOM-спостереження додають походження, не дубль.
func siteDiscoveryLinks(data SEOData, enabled bool, allowPrivate bool) ([]seo.DiscoveredLink, bool) {
	links := make([]seo.DiscoveredLink, 0, min(len(data.DiscoveredLinks), seo.MaxDiscoveredLinks))
	truncated := data.LinksTruncated
	indices := make(map[string]int)
	appendLink := func(link seo.DiscoveredLink, source string) {
		u, _, err := normalizeSiteURL(link.URL, allowPrivate)
		if err != nil {
			return
		}
		if i, exists := indices[u]; exists {
			old := &links[i]
			if old.Source != source {
				old.Source = "both"
			}
			old.Nofollow = old.Nofollow && link.Nofollow
			if old.Anchor == "" {
				old.Anchor = link.Anchor
			}
			return
		}
		if len(links) >= seo.MaxDiscoveredLinks {
			truncated = true
			return
		}
		indices[u] = len(links)
		link.URL, link.Source = u, source
		links = append(links, link)
	}
	for _, link := range data.DiscoveredLinks {
		appendLink(link, "html")
	}
	if !enabled {
		return links, truncated
	}
	r := data.Rendering
	if r == nil || r.Rendered == nil || (r.Status != "completed" && r.Status != "partial") {
		return links, true
	}
	truncated = truncated || r.LinksTruncated || r.Status != "completed" || !r.Rendered.Complete
	for _, link := range r.DiscoveredLinks {
		appendLink(link, "rendered")
	}
	return links, truncated
}
