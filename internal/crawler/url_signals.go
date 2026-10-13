package crawler

import (
	"mime"
	"strings"
)

const URLSignalsVersion = "url-signals-v1"

type URLReference struct {
	Truncated    bool     `json:"truncated,omitempty"`
	URL          string   `json:"url"`
	Fingerprints []string `json:"fingerprints,omitempty"`
	Valid        bool     `json:"valid"`
}

type URLSignals struct {
	Version  string         `json:"version"`
	Complete bool           `json:"complete"`
	HTML     *URLReference  `json:"html,omitempty"`
	HTTP     []URLReference `json:"http,omitempty"`
	Redirect *URLReference  `json:"redirect,omitempty"`
}

// HTTP Link має окремі quoted parameters і URL у кутових дужках; простий split за комою непридатний.
func CanonicalHeaders(headers []string) *URLSignals {
	s := &URLSignals{Version: URLSignalsVersion, Complete: true}
	total := 0
	for _, header := range headers {
		total += len(header)
		if total > 8192 {
			s.Complete = false
			break
		}
		parts, valid := linkHeaderParts(header)
		s.Complete = s.Complete && valid
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			end := strings.IndexByte(part, '>')
			if !strings.HasPrefix(part, "<") || end < 1 {
				s.Complete = false
				continue
			}
			_, params, err := mime.ParseMediaType("application/link" + strings.TrimSpace(part[end+1:]))
			if err != nil {
				s.Complete = false
				continue
			}
			canonical := false
			for _, rel := range strings.Fields(params["rel"]) {
				canonical = canonical || strings.EqualFold(rel, "canonical")
			}
			if !canonical {
				continue
			}
			if _, anchored := params["anchor"]; anchored {
				s.Complete = false
				continue
			}
			if len(s.HTTP) >= 8 || end-1 > 2048 {
				s.Complete = false
				continue
			}
			s.HTTP = append(s.HTTP, URLReference{URL: part[1:end]})
		}
	}
	return s
}

func linkHeaderParts(value string) ([]string, bool) {
	parts := []string{}
	start := 0
	quoted, angle, escaped := false, false, false
	for i, c := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if !angle && c == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			if c == '<' {
				angle = true
			}
			if c == '>' {
				angle = false
			}
			if c == ',' && !angle {
				parts = append(parts, value[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, value[start:])
	return parts, !quoted && !angle && !escaped
}
