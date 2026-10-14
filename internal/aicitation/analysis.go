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

const Model = "local-answer-citations-v2"
const MaxCandidates = 64

type Brand struct{ Name, Domain string }

type AmbiguityRule struct {
	Alias        string   `json:"alias"`
	ContextTerms []string `json:"context_terms"`
}

type Entity struct {
	CanonicalName   string          `json:"canonical_name"`
	Aliases         []string        `json:"aliases,omitempty"`
	EntityType      string          `json:"entity_type"`
	VerifiedDomains []string        `json:"verified_domains,omitempty"`
	SameAsURLs      []string        `json:"same_as_urls,omitempty"`
	AmbiguityRules  []AmbiguityRule `json:"ambiguity_rules,omitempty"`
}

type EntityMatch struct {
	CanonicalName    string   `json:"canonical_name"`
	EntityType       string   `json:"entity_type"`
	Status           string   `json:"status"`
	MatchedAliases   []string `json:"matched_aliases"`
	AmbiguousAliases []string `json:"ambiguous_aliases"`
	SameAsReferenced bool     `json:"same_as_referenced"`
}

type Citation struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Domain   string `json:"domain"`
	Relation string `json:"relation"`
	Source   string `json:"source"`
	Approved bool   `json:"approved"`
}

type Analysis struct {
	Model                   string        `json:"model"`
	BrandMention            bool          `json:"brand_mention"`
	BrandEntity             EntityMatch   `json:"brand_entity"`
	CompetitorMentions      []string      `json:"competitor_mentions"`
	CompetitorEntityMatches []EntityMatch `json:"competitor_entity_matches"`
	Citations               []Citation    `json:"citations"`
	Limited                 bool          `json:"limited"`
	SourceCoverage          string        `json:"source_coverage"`
}

var absoluteURL = regexp.MustCompile(`(?i)https?://[^\s<>"` + "`" + `]+`)
var bareDomain = regexp.MustCompile(`(?i)(?:[\p{L}\p{N}][\p{L}\p{N}-]*\.)+[\p{L}]{2,63}(?:/[^\s<>"` + "`" + `]*)?`)

func normalize(s string) string { return cases.Fold().String(norm.NFKC.String(s)) }
func tokenWords(s string) []string {
	return strings.FieldsFunc(normalize(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

func Mentions(text, brand string) bool {
	return phrasePresent(tokenWords(text), tokenWords(brand))
}

func phrasePresent(words, needle []string) bool {
	if len(needle) == 0 {
		return false
	}
	for i := 0; i+len(needle) <= len(words); i++ {
		if phraseAt(words, i, needle) {
			return true
		}
	}
	return false
}

func phraseAt(words []string, at int, needle []string) bool {
	if at < 0 || at+len(needle) > len(words) {
		return false
	}
	for j := range needle {
		if words[at+j] != needle[j] {
			return false
		}
	}
	return true
}

func entityMatch(text string, entity Entity) EntityMatch {
	result := EntityMatch{CanonicalName: entity.CanonicalName, EntityType: entity.EntityType, Status: "not_found", MatchedAliases: []string{}, AmbiguousAliases: []string{}}
	segments := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == ';' || r == '\n' || r == '\r' || r == '؟' || r == '。'
	})
	aliases := append([]string{entity.CanonicalName}, entity.Aliases...)
	rules := make(map[string][]string, len(entity.AmbiguityRules))
	for _, rule := range entity.AmbiguityRules {
		rules[normalize(rule.Alias)] = tokenWords(strings.Join(rule.ContextTerms, " "))
	}
	matched, ambiguous := map[string]bool{}, map[string]bool{}
	for _, alias := range aliases {
		needle := tokenWords(alias)
		if len(needle) == 0 {
			continue
		}
		key := normalize(alias)
		contextTerms, hasRule := rules[key]
		for _, segment := range segments {
			words := tokenWords(segment)
			for i := 0; i+len(needle) <= len(words); i++ {
				if !phraseAt(words, i, needle) {
					continue
				}
				if !hasRule || contextNear(words, i, len(needle), contextTerms) {
					matched[alias] = true
				} else {
					ambiguous[alias] = true
				}
			}
		}
	}
	for alias := range matched {
		result.MatchedAliases = append(result.MatchedAliases, alias)
	}
	for alias := range ambiguous {
		if !matched[alias] {
			result.AmbiguousAliases = append(result.AmbiguousAliases, alias)
		}
	}
	result.MatchedAliases = sortedUnique(result.MatchedAliases)
	result.AmbiguousAliases = sortedUnique(result.AmbiguousAliases)
	for _, reference := range entity.SameAsURLs {
		if reference != "" && strings.Contains(text, reference) {
			result.SameAsReferenced = true
			break
		}
	}
	switch {
	case len(result.MatchedAliases) > 0:
		result.Status = "matched"
	case len(result.AmbiguousAliases) > 0:
		result.Status = "ambiguous"
	}
	return result
}

func contextNear(words []string, start, phraseLength int, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	lo, hi := start-8, start+phraseLength+8
	if lo < 0 {
		lo = 0
	}
	if hi > len(words) {
		hi = len(words)
	}
	for _, context := range terms {
		for i := lo; i < hi; i++ {
			if words[i] == context {
				return true
			}
		}
	}
	return false
}

func sortedUnique(values []string) []string {
	if len(values) < 2 {
		return values
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && normalize(values[j]) < normalize(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || normalize(value) != normalize(out[len(out)-1]) {
			out = append(out, value)
		}
	}
	return out
}

// Analyze зберігає сумісність із викликами, які мають лише назву та основний домен.
func Analyze(text, sources string, own Brand, competitors []Brand) (Analysis, error) {
	ownEntity := Entity{CanonicalName: own.Name, EntityType: "company", VerifiedDomains: nonEmpty(own.Domain)}
	competitorEntities := make([]Entity, 0, len(competitors))
	for _, competitor := range competitors {
		competitorEntities = append(competitorEntities, Entity{CanonicalName: competitor.Name, EntityType: "company", VerifiedDomains: nonEmpty(competitor.Domain)})
	}
	return AnalyzeEntities(text, sources, ownEntity, competitors, competitorEntities)
}

func AnalyzeEntities(text, sources string, own Entity, competitors []Brand, competitorEntities []Entity) (Analysis, error) {
	a := Analysis{Model: Model, CompetitorMentions: []string{}, CompetitorEntityMatches: []EntityMatch{}, Citations: []Citation{}, SourceCoverage: "unknown"}
	if !utf8.ValidString(text) || !utf8.ValidString(sources) || len(text) > 100000 || len(sources) > 20000 || strings.TrimSpace(text) == "" || strings.ContainsRune(text+sources, 0) || len(competitors) > 5 || len(competitorEntities) > 5 {
		return a, errors.New("відповідь: 1–100000 байтів UTF-8; джерела: до 20000 байтів; без NUL")
	}
	if strings.TrimSpace(sources) != "" {
		a.SourceCoverage = "provided_unreviewed"
	}
	a.BrandEntity = entityMatch(text, own)
	a.BrandMention = a.BrandEntity.Status == "matched"
	for i, b := range competitors {
		entity := Entity{CanonicalName: b.Name, EntityType: "company", VerifiedDomains: nonEmpty(b.Domain)}
		if i < len(competitorEntities) {
			entity = competitorEntities[i]
		}
		match := entityMatch(text, entity)
		a.CompetitorEntityMatches = append(a.CompetitorEntityMatches, match)
		if match.Status == "matched" {
			a.CompetitorMentions = append(a.CompetitorMentions, entity.CanonicalName)
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
		if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Port() != "" {
			return
		}
		host, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")))
		if err != nil || !strings.Contains(host, ".") {
			return
		}
		u.Fragment, u.RawFragment = "", ""
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
		if withinDomains(host, append([]string{ownDomain(own)}, own.VerifiedDomains...)) {
			relation = "own"
		} else {
			for i, competitor := range competitors {
				entity := Entity{VerifiedDomains: nonEmpty(competitor.Domain)}
				if i < len(competitorEntities) {
					entity = competitorEntities[i]
				}
				if withinDomains(host, append([]string{competitor.Domain}, entity.VerifiedDomains...)) {
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
	if a.Limited {
		a.SourceCoverage = "limited"
	}
	return a, nil
}

func nonEmpty(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{value}
}

func ownDomain(own Entity) string {
	if len(own.VerifiedDomains) > 0 {
		return own.VerifiedDomains[0]
	}
	return ""
}

func withinDomains(host string, domains []string) bool {
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
			domain = ascii
		}
		if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
			return true
		}
	}
	return false
}
