package main

import (
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/seo"
)

func secureURLReference(ref crawler.URLReference, cfg Config, canonical ...bool) crawler.URLReference {
	if ref.Truncated {
		ref.Valid = false
		ref.Fingerprints = nil
		ref.URL = sanitizeGEOTextForStorage(redactURL(ref.URL), 2048)
		return ref
	}
	if len(ref.Fingerprints) == 0 {
		ref.Valid = false
		parsed, parseErr := url.Parse(ref.URL)
		validCanonical := len(canonical) == 0 || !canonical[0] || parseErr == nil && parsed.Fragment == "" && parsed.IsAbs()
		if normalized, err := normalizeTargetURL(ref.URL, cfg.AllowPrivateTargets); err == nil && validCanonical {
			ref.Valid = true
			ref.Fingerprints = []string{hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, normalized))}
			u, _ := url.Parse(normalized)
			if u.Path == "" || u.Path == "/" {
				if u.Path == "" {
					u.Path = "/"
				} else {
					u.Path = ""
				}
				ref.Fingerprints = append(ref.Fingerprints, hex.EncodeToString(fingerprintURL(cfg.TargetFingerprintKey, u.String())))
			}
		}
	}
	ref.URL = sanitizeGEOTextForStorage(redactURL(ref.URL), 2048)
	return ref
}

func secureURLSignals(data SEOData, cfg Config) *crawler.URLSignals {
	s := data.URLSignals
	if s == nil && data.CanonicalURL == "" && data.RedirectURL == "" {
		return nil
	}
	result := crawler.URLSignals{Version: crawler.URLSignalsVersion, Complete: true}
	if s != nil {
		result = *s
		result.HTTP = append([]crawler.URLReference(nil), s.HTTP...)
	}
	if data.CanonicalURL != "" && result.HTML == nil {
		result.HTML = &crawler.URLReference{URL: data.CanonicalURL}
	}
	if data.IsRedirect && data.RedirectURL != "" && result.Redirect == nil {
		base, e := url.Parse(data.URL)
		ref, rerr := url.Parse(data.RedirectURL)
		if e == nil && rerr == nil {
			result.Redirect = &crawler.URLReference{URL: base.ResolveReference(ref).String()}
		}
	}
	if data.CanonicalURLTruncated || data.RedirectURLTruncated {
		result.Complete = false
	}
	if data.CanonicalURLTruncated && result.HTML != nil {
		copy := *result.HTML
		copy.Truncated = true
		result.HTML = &copy
	}
	if data.RedirectURLTruncated && result.Redirect != nil {
		copy := *result.Redirect
		copy.Truncated = true
		result.Redirect = &copy
	}
	if data.StatusCode != nil && *data.StatusCode == 200 && !data.HTMLSizeComplete {
		result.Complete = false
	}
	if result.HTML != nil {
		value := secureURLReference(*result.HTML, cfg, true)
		result.HTML = &value
	}
	if result.Redirect != nil {
		value := secureURLReference(*result.Redirect, cfg)
		result.Redirect = &value
	}
	for i := range result.HTTP {
		if len(result.HTTP[i].Fingerprints) == 0 && !result.HTTP[i].Truncated {
			self := seo.IsSelfCanonical(result.HTTP[i].URL, data.URL)
			result.HTTP[i].Self = &self
		}
		result.HTTP[i] = secureURLReference(result.HTTP[i], cfg, true)
	}
	return &result
}

func canonicalHeaderChanged(headers []string, baseline *conditionalBaseline, cfg Config) bool {
	if len(headers) == 0 {
		return false
	}
	if baseline == nil || baseline.Data.URLSignals == nil {
		return true
	}
	current := crawler.CanonicalHeaders(headers)
	if !current.Complete {
		return true
	}
	old := baseline.Data.URLSignals.HTTP
	if len(current.HTTP) != len(old) {
		return true
	}
	for i, ref := range current.HTTP {
		value := secureURLReference(ref, cfg, true)
		if value.Valid != old[i].Valid || strings.Join(value.Fingerprints, ",") != strings.Join(old[i].Fingerprints, ",") {
			return true
		}
	}
	return false
}
