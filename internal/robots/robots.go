package robots

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	MaxPolicyBytes                        = 512 * 1024
	MaxPolicyMatchOperations              = 2_000_000
	policyContextCheckInterval            = 64
	policyMatchContextCheckInterval       = 256
	estimatedPolicyBaseBytes        int64 = 512
	estimatedCompiledRuleBytes      int64 = 64
	estimatedPatternByteFactor      int64 = 2
)

var ErrPolicyMatchComplexity = errors.New("robots.txt policy match complexity exceeds limit")

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
	directive   string
	endAnchored bool
}

// Decision explains the effective robots.txt instruction for one URL.
type Decision struct {
	Allowed           bool
	Directive         string
	ConflictDirective string
	Conflict          bool
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
			directive:   sourceRule.pattern,
			endAnchored: endAnchored,
		})
		policy.estimatedMemoryBytes += estimatedCompiledRuleBytes +
			int64(len(pattern))*estimatedPatternByteFactor
	}

	return policy, nil
}

// CompilePoliciesContext parses robots.txt once, then prepares policies for a
// bounded set of product tokens. Identical selected groups share the same
// immutable Policy. A per-token limit failure is reported only for that token
// so optional diagnostics cannot make the primary crawler fail closed.
func CompilePoliciesContext(ctx context.Context, content string, userAgents []string, maxRules int, maxWeight int64) (map[string]*Policy, map[string]error, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("robots.txt compile context is required")
	}
	if len(content) > MaxPolicyBytes {
		return nil, nil, fmt.Errorf("robots.txt exceeds policy limit of %d bytes", MaxPolicyBytes)
	}
	groups, err := parseGroups(ctx, content)
	if err != nil {
		return nil, nil, err
	}

	policies := make(map[string]*Policy, len(userAgents))
	policyErrors := make(map[string]error)
	shared := make(map[string]*Policy)
	var totalWeight int64
	for _, userAgent := range userAgents {
		if err := ctx.Err(); err != nil {
			policyErrors[userAgent] = fmt.Errorf("compile optional robots.txt policy: %w", err)
			continue
		}
		selected, ok := selectGroup(groups, userAgent)
		if !ok || len(selected.rules) == 0 {
			policies[userAgent] = &Policy{estimatedMemoryBytes: estimatedPolicyBaseBytes}
			continue
		}
		identity := groupRulesIdentity(selected.rules)
		if cached := shared[identity]; cached != nil {
			policies[userAgent] = cached
			continue
		}
		if maxRules > 0 && len(selected.rules) > maxRules {
			policyErrors[userAgent] = fmt.Errorf("selected robots.txt group has %d rules; diagnostic limit is %d", len(selected.rules), maxRules)
			continue
		}

		policy := &Policy{rules: make([]compiledRule, 0, len(selected.rules)), estimatedMemoryBytes: estimatedPolicyBaseBytes}
		for index, sourceRule := range selected.rules {
			if index%policyContextCheckInterval == 0 {
				if err := ctx.Err(); err != nil {
					policyErrors[userAgent] = fmt.Errorf("compile optional robots.txt policy: %w", err)
					break
				}
			}
			pattern, endAnchored, ok := preparePattern(sourceRule.pattern)
			if !ok {
				continue
			}
			policy.rules = append(policy.rules, compiledRule{
				allow: sourceRule.allow, specificity: sourceRule.specificity,
				pattern: pattern, directive: sourceRule.pattern, endAnchored: endAnchored,
			})
			policy.estimatedMemoryBytes += estimatedCompiledRuleBytes + int64(len(pattern))*estimatedPatternByteFactor
		}
		if _, failed := policyErrors[userAgent]; failed {
			continue
		}
		if maxWeight > 0 && totalWeight+policy.estimatedMemoryBytes > maxWeight {
			policyErrors[userAgent] = fmt.Errorf("compiled robots.txt diagnostic policies exceed the %d-byte shared budget", maxWeight)
			continue
		}
		shared[identity] = policy
		policies[userAgent] = policy
		totalWeight += policy.estimatedMemoryBytes
	}
	return policies, policyErrors, nil
}

func groupRulesIdentity(rules []rule) string {
	var key strings.Builder
	for _, item := range rules {
		if item.allow {
			key.WriteByte('A')
		} else {
			key.WriteByte('D')
		}
		key.WriteString(fmt.Sprint(len(item.pattern)))
		key.WriteByte(':')
		key.WriteString(item.pattern)
		key.WriteByte('\x00')
	}
	return key.String()
}

func (p *Policy) EstimatedMemoryBytes() int64 {
	if p == nil {
		return 0
	}
	return p.estimatedMemoryBytes
}

// Allows normalizes and checks a request path against prepared rule matchers.
func (p *Policy) Allows(requestPath string) bool {
	allowed, err := p.AllowsContext(context.Background(), requestPath)
	return err == nil && allowed
}

// AllowsContext checks a request path within the caller's cancellation and complexity budgets.
func (p *Policy) AllowsContext(ctx context.Context, requestPath string) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("robots.txt match context is required")
	}
	return p.allowsNormalizedContext(ctx, normalizeMatchOctets(requestPath))
}

// AllowsURL checks a parsed URL without normalizing its request path twice.
func (p *Policy) AllowsURL(parsed *url.URL) bool {
	allowed, err := p.AllowsURLContext(context.Background(), parsed)
	return err == nil && allowed
}

// AllowsURLContext checks a parsed URL without normalizing its request path twice.
func (p *Policy) AllowsURLContext(ctx context.Context, parsed *url.URL) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("robots.txt match context is required")
	}
	if parsed == nil {
		return false, nil
	}
	return p.allowsNormalizedContext(ctx, RequestPath(parsed))
}

// ExplainURLContext returns the winning directive. Equal-specificity Allow and
// Disallow rules resolve to Allow, as required by the robots matching rules,
// while Conflict makes that surprising configuration visible to callers.
func (p *Policy) ExplainURLContext(ctx context.Context, parsed *url.URL) (Decision, error) {
	if ctx == nil {
		return Decision{}, fmt.Errorf("robots.txt match context is required")
	}
	if parsed == nil {
		return Decision{Allowed: true, Directive: "implicit allow"}, nil
	}
	return p.explainNormalizedContext(ctx, RequestPath(parsed))
}

func (p *Policy) allowsNormalizedContext(ctx context.Context, requestPath string) (bool, error) {
	decision, err := p.explainNormalizedContext(ctx, requestPath)
	return decision.Allowed, err
}

func (p *Policy) explainNormalizedContext(ctx context.Context, requestPath string) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, fmt.Errorf("match robots.txt policy: %w", err)
	}
	if isRobotsTXTRequestPath(requestPath) {
		return Decision{Allowed: true, Directive: "implicit allow: robots.txt is always accessible"}, nil
	}
	if p == nil || len(p.rules) == 0 {
		return Decision{Allowed: true, Directive: "implicit allow"}, nil
	}

	decision := Decision{Allowed: true, Directive: "implicit allow"}
	bestSpecificity := -1
	conflictingAtBestSpecificity := false
	conflictDirective := ""
	remainingOperations := MaxPolicyMatchOperations
	for index, rule := range p.rules {
		if index%policyContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return Decision{}, fmt.Errorf("match robots.txt policy: %w", err)
			}
		}
		matched, err := patternMatchesContext(
			ctx,
			rule.pattern,
			requestPath,
			rule.endAnchored,
			&remainingOperations,
		)
		if err != nil {
			return Decision{}, fmt.Errorf("match robots.txt policy: %w", err)
		}
		if !matched {
			continue
		}
		if rule.specificity > bestSpecificity {
			decision.Allowed = rule.allow
			decision.Directive = directiveText(rule)
			bestSpecificity = rule.specificity
			conflictingAtBestSpecificity = false
			conflictDirective = ""
			continue
		}
		if rule.specificity == bestSpecificity && rule.allow != decision.Allowed {
			conflictingAtBestSpecificity = true
			if rule.allow {
				conflictDirective = decision.Directive
				decision.Allowed = true
				decision.Directive = directiveText(rule)
			} else {
				conflictDirective = directiveText(rule)
			}
		}
	}
	decision.Conflict = conflictingAtBestSpecificity
	decision.ConflictDirective = conflictDirective
	return decision, nil
}

func directiveText(rule compiledRule) string {
	name := "Disallow"
	if rule.allow {
		name = "Allow"
	}
	return name + ": " + rule.directive
}

func isRobotsTXTRequestPath(requestPath string) bool {
	path, _, _ := strings.Cut(requestPath, "?")
	return path == "/robots.txt"
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
	// Applebot uses Googlebot rules when no Applebot-specific group exists.
	// A wildcard group does not take precedence over that provider fallback.
	if strings.EqualFold(strings.TrimSpace(strings.SplitN(userAgent, "/", 2)[0]), "Applebot") {
		if selected, ok := selectMatchingGroup(groups, "Applebot", false); ok {
			return selected, true
		}
		if selected, ok := selectMatchingGroup(groups, "Googlebot", false); ok {
			return selected, true
		}
	}
	return selectMatchingGroup(groups, userAgent, true)
}

func selectMatchingGroup(groups []group, userAgent string, includeWildcard bool) (group, bool) {
	bestLength := -1
	var selected group
	found := false

	for _, group := range groups {
		groupBestLength := -1
		for _, agent := range group.agents {
			if agent == "*" && !includeWildcard {
				continue
			}
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

func patternMatchesContext(
	ctx context.Context,
	pattern string,
	requestPath string,
	endAnchored bool,
	remainingOperations *int,
) (bool, error) {
	if !endAnchored {
		pattern += "*"
	}

	patternIndex := 0
	pathIndex := 0
	starIndex := -1
	starPathIndex := 0
	for pathIndex < len(requestPath) {
		if err := consumeMatchOperation(ctx, remainingOperations); err != nil {
			return false, err
		}
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
			return false, nil
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		if err := consumeMatchOperation(ctx, remainingOperations); err != nil {
			return false, err
		}
		patternIndex++
	}
	return patternIndex == len(pattern), nil
}

func consumeMatchOperation(ctx context.Context, remainingOperations *int) error {
	if *remainingOperations <= 0 {
		return ErrPolicyMatchComplexity
	}
	*remainingOperations--
	if *remainingOperations%policyMatchContextCheckInterval == 0 {
		return ctx.Err()
	}
	return nil
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
