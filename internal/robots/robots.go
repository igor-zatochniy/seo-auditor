package robots

import (
	"net/url"
	"regexp"
	"strings"
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

func RequestPath(parsed *url.URL) string {
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return path
}

func IsPathAllowed(content, userAgent, requestPath string) bool {
	groups := parseGroups(content)
	group, ok := selectGroup(groups, userAgent)
	if !ok || len(group.rules) == 0 {
		return true
	}

	allowed := true
	bestSpecificity := -1
	for _, rule := range group.rules {
		if !patternMatches(rule.pattern, requestPath) {
			continue
		}
		if rule.specificity > bestSpecificity || (rule.specificity == bestSpecificity && rule.allow) {
			allowed = rule.allow
			bestSpecificity = rule.specificity
		}
	}

	return allowed
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
	cleaned := strings.NewReplacer("*", "", "$", "").Replace(pattern)
	return len(cleaned)
}

func patternMatches(pattern, requestPath string) bool {
	if pattern == "" {
		return false
	}

	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}

	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
	if endAnchored {
		expr += "$"
	}

	matched, err := regexp.MatchString(expr, requestPath)
	return err == nil && matched
}
