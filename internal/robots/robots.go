package robots

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	MaxPolicyBytes                   = 512 * 1024
	policyContextCheckInterval       = 64
	estimatedPolicyBaseBytes   int64 = 512
	estimatedCompiledRuleBytes int64 = 64
	estimatedPatternByteFactor int64 = 2
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
	pattern     string
	endAnchored bool
}

// Policy is an immutable robots.txt policy safe for concurrent path checks.
type Policy struct {
	rules                []compiledRule
	estimatedMemoryBytes int64
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
	return CompilePolicyContext(context.Background(), content, userAgent)
}

func CompilePolicyContext(ctx context.Context, content, userAgent string) (*Policy, error) {
	if ctx == nil {
		return nil, fmt.Errorf("robots.txt compile context is required")
	}
	if len(content) > MaxPolicyBytes {
		return nil, fmt.Errorf("robots.txt exceeds policy limit of %d bytes", MaxPolicyBytes)
	}

	groups, err := parseGroups(ctx, content)
	if err != nil {
		return nil, err
	}
	group, ok := selectGroup(groups, userAgent)
	if !ok || len(group.rules) == 0 {
		return &Policy{estimatedMemoryBytes: estimatedPolicyBaseBytes}, nil
	}

	policy := &Policy{
		rules:                make([]compiledRule, 0, len(group.rules)),
		estimatedMemoryBytes: estimatedPolicyBaseBytes,
	}
	for index, sourceRule := range group.rules {
		if index%policyContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("compile robots.txt policy: %w", err)
			}
		}
		pattern, endAnchored, ok := preparePattern(sourceRule.pattern)
		if !ok {
			continue
		}
		policy.rules = append(policy.rules, compiledRule{
			allow:       sourceRule.allow,
			specificity: sourceRule.specificity,
			pattern:     pattern,
			endAnchored: endAnchored,
		})
		policy.estimatedMemoryBytes += estimatedCompiledRuleBytes +
			int64(len(pattern))*estimatedPatternByteFactor
	}

	return policy, nil
}

func (p *Policy) EstimatedMemoryBytes() int64 {
	if p == nil {
		return 0
	}
	return p.estimatedMemoryBytes
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
		if !patternMatches(rule.pattern, requestPath, rule.endAnchored) {
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

func parseGroups(ctx context.Context, content string) ([]group, error) {
	var groups []group
	var current group
	seenRule := false

	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	for lineIndex, line := range strings.Split(content, "\n") {
		if lineIndex%policyContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("parse robots.txt policy: %w", err)
			}
		}
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

	return groups, nil
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

	product := strings.TrimSpace(strings.SplitN(userAgent, "/", 2)[0])
	return strings.EqualFold(agent, product)
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

func preparePattern(pattern string) (string, bool, bool) {
	if pattern == "" {
		return "", false, false
	}
	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	return normalizeMatchOctets(pattern), endAnchored, true
}

func patternMatches(pattern, requestPath string, endAnchored bool) bool {
	if !endAnchored {
		pattern += "*"
	}

	patternIndex := 0
	pathIndex := 0
	starIndex := -1
	starPathIndex := 0
	for pathIndex < len(requestPath) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == requestPath[pathIndex]:
			patternIndex++
			pathIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			patternIndex++
			starPathIndex = pathIndex
		case starIndex >= 0:
			patternIndex = starIndex + 1
			starPathIndex++
			pathIndex = starPathIndex
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
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
