package international

import (
	"slices"
	"strings"

	"golang.org/x/text/language"
)

const MaxAlternates = 64

// Evidence містить лише безпечні URL та псевдонімізовані ідентифікатори після збереження.
type Evidence struct {
	Links                []Alternate `json:"links"`
	Complete             bool        `json:"complete"`
	CanonicalFingerprint string      `json:"canonical_fingerprint,omitempty"`
}

type Alternate struct {
	Language      string `json:"language"`
	URL           string `json:"url"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Absolute      bool   `json:"absolute"`
	ValidURL      bool   `json:"valid_url"`
	LanguageValid bool   `json:"language_valid"`
}

type Observation struct {
	HTTP                 *int   `json:"http"`
	Status               string `json:"status"`
	Reciprocal           *bool  `json:"reciprocal"`
	CanonicalSelf        *bool  `json:"canonical_self"`
	CanonicalFingerprint string `json:"canonical_fingerprint,omitempty"`
}

type Issue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
	Language string `json:"language,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Page struct {
	TargetID     int64                  `json:"target_id"`
	URL          string                 `json:"url"`
	HTTP         *int                   `json:"http"`
	HTML         *Evidence              `json:"html"`
	Sitemap      *Evidence              `json:"sitemap"`
	Observations map[string]Observation `json:"observations"`
	Issues       []Issue                `json:"issues"`
	Status       string                 `json:"status"`
	Fingerprint  string                 `json:"-"`
}

// ValidLanguage приймає ISO 639-1, необов'язковий ISO 15924 та ISO 3166-1 alpha-2.
func ValidLanguage(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "x-default" {
		return true
	}
	parts := strings.Split(value, "-")
	if len(parts) < 1 || len(parts) > 3 || len(parts[0]) != 2 {
		return false
	}
	b, err := language.ParseBase(parts[0])
	if err != nil || b.String() != parts[0] || b.String() == "und" {
		return false
	}
	i := 1
	if i < len(parts) && len(parts[i]) == 4 {
		s, err := language.ParseScript(parts[i])
		if err != nil || strings.EqualFold(s.String(), "Zzzz") {
			return false
		}
		i++
	}
	if i < len(parts) {
		v := strings.ToUpper(parts[i])
		if len(v) != 2 || v == "UK" || v == "EU" || v == "UN" || v == "ZZ" {
			return false
		}
		r, err := language.ParseRegion(v)
		if err != nil || r.String() != v || !r.IsCountry() {
			return false
		}
		i++
	}
	return i == len(parts)
}

func Diagnose(p *Page) {
	p.Issues = []Issue{}
	p.Status = "not_checked"
	if p.HTML == nil && p.Sitemap == nil {
		return
	}
	p.Status = "ok"
	add := func(code, severity, message, source string, a Alternate) {
		p.Issues = append(p.Issues, Issue{code, severity, message, source, a.Language, a.URL})
		if severity == "error" {
			p.Status = "error"
		} else if p.Status == "ok" {
			p.Status = "review"
		}
	}
	for _, source := range []struct {
		name string
		e    *Evidence
	}{{"html", p.HTML}, {"sitemap", p.Sitemap}} {
		e := source.e
		if e == nil {
			continue
		}
		if !e.Complete {
			add("incomplete_evidence", "review", "Збір обмежений: відсутність сигналу не є доведеною помилкою.", source.name, Alternate{})
		}
		if len(e.Links) == 0 {
			continue
		}
		self, fallback := false, false
		languages := map[string]string{}
		for _, a := range e.Links {
			lang := strings.ToLower(strings.TrimSpace(a.Language))
			if !ValidLanguage(lang) {
				add("invalid_language", "error", "Некоректний мовний, регіональний або script-код hreflang.", source.name, a)
			}
			if !a.Absolute {
				add("relative_alternate", "error", "Hreflang потребує повного абсолютного HTTP(S) URL.", source.name, a)
			}
			if !a.ValidURL {
				add("invalid_alternate_url", "error", "Некоректний або небезпечний URL альтернативи.", source.name, a)
				continue
			}
			if prior, ok := languages[lang]; ok && prior != a.Fingerprint {
				add("conflicting_language", "error", "Один hreflang-код указує на різні URL.", source.name, a)
			}
			languages[lang] = a.Fingerprint
			self = self || a.Fingerprint == p.Fingerprint
			fallback = fallback || lang == "x-default"
			o, ok := p.Observations[a.Fingerprint]
			if !ok || o.HTTP == nil {
				add("alternate_not_checked", "review", "HTTP і взаємність не підтверджені в цьому аудиті.", source.name, a)
				continue
			}
			if *o.HTTP != 200 {
				add("alternate_http", "error", "Альтернатива повернула не HTTP 200.", source.name, a)
				continue
			}
			if o.Status != "completed" {
				add("alternate_not_checked", "review", "HTML альтернативи не отримано повністю; взаємність не підтверджена.", source.name, a)
				continue
			}
			if o.Reciprocal == nil {
				add("reciprocity_not_checked", "review", "Немає повних даних для перевірки зворотного hreflang.", source.name, a)
			} else if !*o.Reciprocal {
				add("missing_return_link", "error", "Альтернатива не містить зворотного hreflang на цю сторінку.", source.name, a)
			}
			if o.CanonicalSelf != nil && !*o.CanonicalSelf {
				severity := canonicalSeverity(a.Fingerprint, o.CanonicalFingerprint, p.HTML, p.Sitemap)
				add("alternate_canonical_conflict", severity, "Canonical альтернативи вказує на іншу сторінку; перевірте узгодженість мовних версій.", source.name, a)
			}
		}
		if e.Complete && !self {
			add("missing_self_reference", "error", "Набір hreflang не містить самопосилання.", source.name, Alternate{})
		}
		if e.Complete && !fallback {
			add("missing_x_default", "review", "Розгляньте x-default для користувачів без відповідної мовної версії; це не обов'язковий тег.", source.name, Alternate{})
		}
	}
	if p.HTML != nil && p.HTML.CanonicalFingerprint != "" && p.HTML.CanonicalFingerprint != p.Fingerprint && (hasLinks(p.HTML) || hasLinks(p.Sitemap)) {
		add("source_canonical_conflict", canonicalSeverity(p.Fingerprint, p.HTML.CanonicalFingerprint, p.HTML, p.Sitemap), "Canonical цієї hreflang-сторінки вказує на інший URL; перевірте узгодженість мовних версій.", "html", Alternate{})
	}
	if hasLinks(p.HTML) && hasLinks(p.Sitemap) && p.HTML.Complete && p.Sitemap.Complete && !slices.Equal(linkSet(p.HTML), linkSet(p.Sitemap)) {
		add("html_sitemap_mismatch", "error", "HTML і sitemap оголошують різні набори hreflang.", "html+sitemap", Alternate{})
	}
}

func canonicalSeverity(from, to string, sources ...*Evidence) string {
	languages := func(fp string) map[string]bool {
		out := map[string]bool{}
		for _, e := range sources {
			if e == nil {
				continue
			}
			for _, a := range e.Links {
				if a.Fingerprint == fp && a.Absolute && a.ValidURL && ValidLanguage(a.Language) && !strings.EqualFold(a.Language, "x-default") {
					base, _, _ := strings.Cut(strings.ToLower(a.Language), "-")
					out[base] = true
				}
			}
		}
		return out
	}
	a, b := languages(from), languages(to)
	if len(a) == 0 || len(b) == 0 {
		return "review"
	}
	for language := range a {
		if b[language] {
			return "review"
		}
	}
	return "error"
}

func hasLinks(e *Evidence) bool { return e != nil && len(e.Links) > 0 }
func linkSet(e *Evidence) []string {
	m := map[string]bool{}
	for _, a := range e.Links {
		m[strings.ToLower(a.Language)+" "+a.Fingerprint] = true
	}
	s := make([]string, 0, len(m))
	for v := range m {
		s = append(s, v)
	}
	slices.Sort(s)
	return s
}
