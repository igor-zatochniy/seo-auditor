package crawler

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Shape зберігає лише форму URL, не значення query-параметрів.
type URLShape struct {
	Version  string   `json:"version"`
	Family   string   `json:"family"`
	Keys     []string `json:"keys"`
	HasQuery bool     `json:"has_query"`
	Page     *int64   `json:"page_bucket,omitempty"`
	Complete bool     `json:"complete"`
}

func DescribeURL(raw string) URLShape {
	s := URLShape{Version: "url-shape-v1", Keys: []string{}, Complete: true}
	if len(raw) > 2048 {
		s.Complete = false
		return s
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil {
		s.Complete = false
		return s
	}
	u.Fragment, u.RawFragment = "", ""
	s.HasQuery = u.RawQuery != ""
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		s.Complete = false
	}
	u.RawQuery, u.ForceQuery = "", false
	s.Family = u.String()
	if len(s.Family) > 2048 {
		s.Family = ""
		s.Complete = false
	}
	for k, values := range q {
		k = strings.ToLower(k)
		valid := len(k) > 0 && len(k) <= 64
		for _, r := range k {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				valid = false
			}
		}
		if !valid {
			s.Complete = false
			continue
		}
		s.Keys = append(s.Keys, k)
		if (k == "page" || k == "p" || k == "offset" || k == "start") && len(values) == 1 {
			n, err := strconv.ParseInt(values[0], 10, 64)
			if err == nil && n >= 0 && n <= 1000000000 {
				// Діапазон не дозволяє відновити вихідне числове значення.
				switch {
				case n <= 10:
					n = 10
				case n <= 100:
					n = 100
				case n < 1000:
					n = 999
				default:
					n = 1000
				}
				if s.Page == nil || n > *s.Page {
					s.Page = &n
				}
			}
		}
	}
	sort.Strings(s.Keys)
	if len(s.Keys) > 16 {
		s.Keys = s.Keys[:16]
		s.Complete = false
	}
	encoded, _ := json.Marshal(s)
	if len(encoded) > 4096 {
		s.Family = ""
		s.Complete = false
	}
	return s
}
