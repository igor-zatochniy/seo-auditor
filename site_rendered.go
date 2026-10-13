package main

import "github.com/igor-zatochniy/seo-auditor/internal/seo"

// Зберігаємо HTML-посилання; однакові DOM-спостереження додають походження, не дубль.
func siteDiscoveryLinks(data SEOData, enabled bool, allowPrivate bool) ([]seo.DiscoveredLink, bool) {
	links := append([]seo.DiscoveredLink(nil), data.DiscoveredLinks...)
	for i := range links {
		links[i].Source = "html"
	}
	truncated := data.LinksTruncated
	if !enabled {
		return links, truncated
	}
	r := data.Rendering
	if r == nil || r.Rendered == nil || (r.Status != "completed" && r.Status != "partial") {
		return links, true
	}
	truncated = truncated || r.LinksTruncated || r.Status != "completed" || !r.Rendered.Complete
	type identity struct {
		url, anchor string
		nofollow    bool
	}
	key := func(link seo.DiscoveredLink) identity {
		u, _, err := normalizeSiteURL(link.URL, allowPrivate)
		if err != nil {
			u = link.URL
		}
		return identity{u, link.Anchor, link.Nofollow}
	}
	available := map[identity][]int{}
	for i, link := range links {
		k := key(link)
		available[k] = append(available[k], i)
	}
	for _, link := range r.DiscoveredLinks {
		k := key(link)
		if indices := available[k]; len(indices) > 0 {
			links[indices[0]].Source = "both"
			available[k] = indices[1:]
			continue
		}
		if len(links) >= seo.MaxDiscoveredLinks {
			truncated = true
			continue
		}
		link.Source = "rendered"
		links = append(links, link)
	}
	return links, truncated
}
