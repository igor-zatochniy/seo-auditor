package seo

import (
	"bytes"
	"net/url"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/international"
)

func (p *pageParser) collectHreflang(name []byte, a tagAttributes) {
	if !bytes.Equal(name, []byte("link")) || !p.inDocumentHead() || len(a.hreflang) == 0 || !hasTokenFold(a.rel, []byte("alternate")) {
		return
	}
	e := p.data.International
	if len(e.Links) >= international.MaxAlternates {
		e.Complete = false
		return
	}
	if len(a.hreflang) > 64 || len(a.href) > 2048 {
		e.Complete = false
		return
	}
	v := international.Alternate{Language: strings.TrimSpace(string(a.hreflang)), URL: strings.TrimSpace(string(a.href))}
	u, err := url.Parse(v.URL)
	if err == nil && v.URL != "" {
		v.Absolute = u.IsAbs() && u.Host != ""
		if p.documentBaseURL != nil {
			u = p.documentBaseURL.ResolveReference(u)
		}
		v.ValidURL = (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Fragment == ""
		if v.ValidURL {
			u.Host = normalizedAuthority(u)
			v.URL = u.String()
			if len(v.URL) > 2048 {
				e.Complete = false
				return
			}
		}
	}
	e.Links = append(e.Links, v)
}
