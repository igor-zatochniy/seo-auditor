package robots

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

type rule struct {
	allow       bool
	pattern     string
	specificity int
}

type group struct {
	agents []string
	rules  []rule
}

type compiledRule struct {
	allow       bool
	specificity int
	matcher     *regexp.Regexp
}

// Policy is an immutable robots.txt policy safe for concurrent path checks.
type Policy struct {
	rules []compiledRule
}

func RequestPath(parsed *url.URL) string {
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return normalizeMatchOctets(path)
}

// CompilePolicy selects the applicable user-agent group and compiles its rule matchers once.
func CompilePolicy(content, userAgent string) (*Policy, error) {
	groups := parseGroups(content)
	group, ok := selectGroup(groups, userAgent)
	if !ok || len(group.rules) == 0 {
		return &Policy{}, nil
	}

	policy := &Policy{
		rules: make([]compiledRule, 0, len(group.rules)),
	}
	for _, sourceRule := range group.rules {
		matcher, err := compilePattern(sourceRule.pattern)
		if err != nil {
			return nil, fmt.Errorf("compile robots.txt rule %q: %w", sourceRule.pattern, err)
		}
		if matcher == nil {
			continue
		}
		policy.rules = append(policy.rules, compiledRule{
			allow:       sourceRule.allow,
			specificity: sourceRule.specificity,
			matcher:     matcher,
		})
	}

	return policy, nil
}

// Allows normalizes and checks a request path against prepared rule matchers.
func (p *Policy) Allows(requestPath string) bool {
	return p.allowsNormalized(normalizeMatchOctets(requestPath))
}

// AllowsURL checks a parsed URL without normalizing its request path twice.
func (p *Policy) AllowsURL(parsed *url.URL) bool {
	if parsed == nil {
		return false
	}
	return p.allowsNormalized(RequestPath(parsed))
}

func (p *Policy) allowsNormalized(requestPath string) bool {
	if p == nil || len(p.rules) == 0 {
		return true
	}

	allowed := true
	bestSpecificity := -1
	for _, rule := range p.rules {
		if !rule.matcher.MatchString(requestPath) {
			continue
		}
		if rule.specificity > bestSpecificity || (rule.specificity == bestSpecificity && rule.allow) {
			allowed = rule.allow
			bestSpecificity = rule.specificity
		}
	}

	return allowed
}

func IsPathAllowed(content, userAgent, requestPath string) bool {
	policy, err := CompilePolicy(content, userAgent)
	return err == nil && policy.Allows(requestPath)
}

func parseGroups(content string) []group {
	var groups []group
	var current group
	seenRule := false

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(stripComment(line))
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if len(current.agents) > 0 && seenRule {
				groups = append(groups, current)
				current = group{}
				seenRule = false
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "allow", "disallow":
			if len(current.agents) == 0 {
				continue
			}
			seenRule = true
			if key == "disallow" && value == "" {
				continue
			}
			current.rules = append(current.rules, rule{
				allow:       key == "allow",
				pattern:     value,
				specificity: ruleSpecificity(value),
			})
		}
	}

	if len(current.agents) > 0 {
		groups = append(groups, current)
	}

	return groups
}

func stripComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

func selectGroup(groups []group, userAgent string) (group, bool) {
	bestLength := -1
	var selected group
	found := false

	for _, group := range groups {
		groupBestLength := -1
		for _, agent := range group.agents {
			if agentMatches(agent, userAgent) && len(agent) > groupBestLength {
				groupBestLength = len(agent)
			}
		}

		if groupBestLength < 0 {
			continue
		}
		if groupBestLength > bestLength {
			selected = group
			bestLength = groupBestLength
			found = true
			continue
		}
		if groupBestLength == bestLength {
			selected.rules = append(selected.rules, group.rules...)
		}
	}

	return selected, found
}

func agentMatches(agent, userAgent string) bool {
	agent = strings.TrimSpace(strings.ToLower(agent))
	if agent == "" {
		return false
	}
	if agent == "*" {
		return true
	}

	ua := strings.ToLower(userAgent)
	product := strings.SplitN(ua, "/", 2)[0]
	return strings.Contains(ua, agent) || strings.Contains(product, agent)
}

func ruleSpecificity(pattern string) int {
	pattern = strings.TrimSuffix(pattern, "$")

	specificity := 0
	for index := 0; index < len(pattern); {
		if pattern[index] == '*' {
			index++
			continue
		}
		if pattern[index] == '%' && index+2 < len(pattern) &&
			isHexDigit(pattern[index+1]) && isHexDigit(pattern[index+2]) {
			specificity++
			index += 3
			continue
		}

		_, size := utf8.DecodeRuneInString(pattern[index:])
		if size == 0 {
			break
		}
		specificity += size
		index += size
	}
	return specificity
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}

	pattern = normalizeMatchOctets(pattern)
	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
	if endAnchored {
		expr += "$"
	}
	return regexp.Compile(expr)
}

func normalizeMatchOctets(value string) string {
	var normalized strings.Builder
	normalized.Grow(len(value))

	for index := 0; index < len(value); {
		if value[index] == '%' && index+2 < len(value) &&
			isHexDigit(value[index+1]) && isHexDigit(value[index+2]) {
			octet := hexValue(value[index+1])<<4 | hexValue(value[index+2])
			if isUnreservedASCII(octet) {
				normalized.WriteByte(octet)
			} else {
				writePercentEncodedByte(&normalized, octet)
			}
			index += 3
			continue
		}

		if value[index] < utf8.RuneSelf {
			normalized.WriteByte(value[index])
			index++
			continue
		}

		_, size := utf8.DecodeRuneInString(value[index:])
		if size == 0 {
			break
		}
		for _, octet := range []byte(value[index : index+size]) {
			writePercentEncodedByte(&normalized, octet)
		}
		index += size
	}

	return normalized.String()
}

func isUnreservedASCII(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}

func isHexDigit(value byte) bool {
	return value >= '0' && value <= '9' ||
		value >= 'a' && value <= 'f' ||
		value >= 'A' && value <= 'F'
}

func hexValue(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	default:
		return value - 'A' + 10
	}
}

func writePercentEncodedByte(builder *strings.Builder, value byte) {
	const upperHex = "0123456789ABCDEF"
	builder.WriteByte('%')
	builder.WriteByte(upperHex[value>>4])
	builder.WriteByte(upperHex[value&0x0f])
}
