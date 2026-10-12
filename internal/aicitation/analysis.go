package aicitation

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const Model = "local-answer-citations-v1"
const MaxCandidates = 64

type Brand struct{ Name, Domain string }
type Citation struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Domain   string `json:"domain"`
	Relation string `json:"relation"`
	Source   string `json:"source"`
	Approved bool   `json:"approved"`
}
type Analysis struct {
	Model              string     `json:"model"`
	BrandMention       bool       `json:"brand_mention"`
	CompetitorMentions []string   `json:"competitor_mentions"`
	Citations          []Citation `json:"citations"`
	Limited            bool       `json:"limited"`
}

var absoluteURL = regexp.MustCompile(`(?i)https?://[^\s<>"` + "`" + `]+`)
var bareDomain = regexp.MustCompile(`(?i)(?:[\p{L}\p{N}][\p{L}\p{N}-]*\.)+[\p{L}]{2,63}(?:/[^\s<>"` + "`" + `]*)?`)

func normalize(s string) string { return cases.Fold().String(norm.NFKC.String(s)) }
func tokenWords(s string) []string {
	return strings.FieldsFunc(normalize(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

func Mentions(text, brand string) bool {
	needle := tokenWords(brand)
	if len(needle) == 0 {
		return false
	}
	words := tokenWords(text)
	for i := 0; i+len(needle) <= len(words); i++ {
		match := true
		for j := range needle {
			if words[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Лише розбір тексту. Жоден розпізнаний URL не завантажується.
func Analyze(text, sources string, own Brand, competitors []Brand) (Analysis, error) {
	a := Analysis{Model: Model, CompetitorMentions: []string{}, Citations: []Citation{}}
	if !utf8.ValidString(text) || !utf8.ValidString(sources) || len(text) > 100000 || len(sources) > 20000 || strings.TrimSpace(text) == "" || strings.ContainsRune(text+sources, 0) || len(competitors) > 5 {
		return a, errors.New("відповідь: 1–100000 байтів UTF-8; джерела: до 20000 байтів; без NUL")
	}
	a.BrandMention = Mentions(text, own.Name)
	for _, b := range competitors {
		if Mentions(text, b.Name) {
			a.CompetitorMentions = append(a.CompetitorMentions, b.Name)
		}
	}
	seen := map[string]bool{}
	add := func(raw, source string) {
		raw = strings.TrimRight(raw, ".,;:!?)]}")
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		if len(raw) > 2048 {
			a.Limited = true
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return
		}
		host, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")))
		if err != nil || !strings.Contains(host, ".") {
			return
		}
		u.Fragment, u.RawFragment = "", ""
		if u.Port() != "" {
			return
		}
		u.Host = host
		value := u.String()
		if seen[value] {
			return
		}
		seen[value] = true
		if len(a.Citations) == MaxCandidates {
			a.Limited = true
			return
		}
		relation := "external"
		within := func(domain string) bool {
			return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
		}
		if within(own.Domain) {
			relation = "own"
		} else {
			for _, b := range competitors {
				if within(b.Domain) {
					relation = "competitor"
					break
				}
			}
		}
		a.Citations = append(a.Citations, Citation{URL: value, Domain: host, Relation: relation, Source: source})
	}
	for _, part := range []struct{ text, source string }{{sources, "sources"}, {text, "answer"}} {
		locations := absoluteURL.FindAllStringIndex(part.text, MaxCandidates+1)
		if len(locations) > MaxCandidates {
			a.Limited = true
		}
		for _, at := range locations {
			add(part.text[at[0]:at[1]], part.source)
		}
		domains := bareDomain.FindAllStringIndex(part.text, MaxCandidates+1)
		if len(domains) > MaxCandidates {
			a.Limited = true
		}
		for _, at := range domains {
			inside := false
			for _, u := range locations {
				if at[0] >= u[0] && at[0] < u[1] {
					inside = true
					break
				}
			}
			if inside || at[0] > 0 && part.text[at[0]-1] == '@' {
				continue
			}
			add(part.text[at[0]:at[1]], part.source)
		}
	}
	return a, nil
}
