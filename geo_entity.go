package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/aicitation"
	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func defaultGEOEntity(name, domain string) aicitation.Entity {
	entity := aicitation.Entity{CanonicalName: strings.TrimSpace(name), EntityType: "company"}
	if strings.TrimSpace(domain) != "" {
		entity.VerifiedDomains = []string{domain}
	}
	return entity
}

func validateGEOEntity(entity aicitation.Entity, fallbackName, fallbackDomain string) (aicitation.Entity, error) {
	if strings.TrimSpace(entity.CanonicalName) == "" {
		entity.CanonicalName = fallbackName
	}
	entity.CanonicalName = sanitizeGEOTextForStorage(entity.CanonicalName, 120)
	if entity.CanonicalName != "" && !validEntityText(entity.CanonicalName, 120) {
		return entity, errors.New("потрібна назва сутності до 120 символів")
	}
	if entity.EntityType == "" {
		entity.EntityType = "company"
	}
	switch entity.EntityType {
	case "company", "product", "person":
	default:
		return entity, errors.New("тип сутності має бути company, product або person")
	}
	if len(entity.Aliases) > 50 || len(entity.VerifiedDomains) > 20 || len(entity.SameAsURLs) > 20 || len(entity.AmbiguityRules) > 50 {
		return entity, errors.New("перевищено ліміт псевдонімів, доменів, sameAs або правил неоднозначності")
	}
	if len(entity.VerifiedDomains) == 0 && strings.TrimSpace(fallbackDomain) != "" {
		entity.VerifiedDomains = []string{fallbackDomain}
	}
	var err error
	entity.VerifiedDomains, err = normalizeEntityDomains(entity.VerifiedDomains)
	if err != nil {
		return entity, err
	}
	if len(entity.VerifiedDomains) == 0 {
		return entity, errors.New("додайте щонайменше один підтверджений домен")
	}
	entity.Aliases, err = normalizeEntityTexts(entity.Aliases, 120)
	if err != nil {
		return entity, err
	}
	entity.SameAsURLs, err = normalizeSameAsURLs(entity.SameAsURLs)
	if err != nil {
		return entity, err
	}
	knownAliases := map[string]bool{}
	allAliases := append([]string(nil), entity.Aliases...)
	if entity.CanonicalName != "" {
		allAliases = append(allAliases, entity.CanonicalName)
	}
	for _, alias := range allAliases {
		knownAliases[entityKey(alias)] = true
	}
	rules := make([]aicitation.AmbiguityRule, 0, len(entity.AmbiguityRules))
	ruleAliases := map[string]bool{}
	for _, rule := range entity.AmbiguityRules {
		rule.Alias = sanitizeGEOTextForStorage(strings.TrimSpace(rule.Alias), 120)
		key := entityKey(rule.Alias)
		if !validEntityText(rule.Alias, 120) || !knownAliases[key] || ruleAliases[key] || len(rule.ContextTerms) == 0 || len(rule.ContextTerms) > 10 {
			return entity, errors.New("правило неоднозначності має посилатися на псевдонім і містити від 1 до 10 контекстних слів")
		}
		ruleAliases[key] = true
		rule.ContextTerms, err = normalizeEntityTexts(rule.ContextTerms, 64)
		if err != nil || len(rule.ContextTerms) == 0 {
			return entity, errors.New("контекстні слова мають бути коректними й непорожніми")
		}
		rules = append(rules, rule)
	}
	entity.AmbiguityRules = rules
	encoded, err := json.Marshal(entity)
	if err != nil || len(encoded) > 24000 {
		return entity, errors.New("реєстр сутності перевищує безпечний ліміт 24 КіБ")
	}
	return entity, nil
}

func validEntityText(value string, maxRunes int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) })
}

func entityKey(value string) string { return cases.Fold().String(norm.NFKC.String(value)) }

func normalizeEntityTexts(values []string, maxRunes int) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		value := sanitizeGEOTextForStorage(strings.TrimSpace(raw), maxRunes)
		if !validEntityText(value, maxRunes) {
			return nil, errors.New("псевдонім або контекстне слово має некоректний формат")
		}
		key := entityKey(value)
		if !seen[key] {
			seen[key] = true
			out = append(out, value)
		}
	}
	return out, nil
}

func normalizeEntityDomains(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		domain, err := normalizeEntityDomain(value)
		if err != nil {
			return nil, err
		}
		if !seen[domain] {
			seen[domain] = true
			out = append(out, domain)
		}
	}
	return out, nil
}

func normalizeEntityDomain(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "/\\:@?# \t\r\n") {
		return "", errors.New("домен має бути вказаний без схеми, порту або шляху")
	}
	host, err := idna.Lookup.ToASCII(strings.ToLower(strings.TrimSuffix(value, ".")))
	if err != nil || len(host) > 253 || !strings.Contains(host, ".") {
		return "", errors.New("некоректний домен сутності")
	}
	return host, nil
}

func normalizeSameAsURLs(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		if len(raw) > 2048 {
			return nil, errors.New("sameAs URL перевищує 2048 байтів")
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("sameAs має бути HTTP(S)-URL без облікових даних, порту, query та fragment")
		}
		host, err := normalizeEntityDomain(u.Hostname())
		if err != nil {
			return nil, err
		}
		u.Host = host
		value := u.String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out, nil
}
