package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	robotsparser "github.com/igor-zatochniy/seo-auditor/internal/robots"
)

const (
	robotsErrorCacheTTL        = time.Minute
	robotsAllowAllPolicyWeight = int64(256)
	maxAIBotPolicyRules        = 4096
	maxAIBotPolicyWeight       = int64(2 * 1024 * 1024)
	MaxRobotsBodyBytes         = int64(robotsparser.MaxPolicyBytes)
)

type robotsPolicy struct {
	allowAll     bool
	compiled     *robotsparser.Policy
	googlebot    *robotsparser.Policy
	oaiSearchBot *robotsparser.Policy
	gptBot       *robotsparser.Policy
	aiPolicies   map[string]*robotsparser.Policy
	aiErrors     map[string]string
	robotsURL    string
	checkedAt    time.Time
	httpStatus   int
	sitemaps     []string
}

func (p robotsPolicy) allows(target *url.URL) bool {
	allowed, err := p.allowsContext(context.Background(), target)
	return err == nil && allowed
}

func (p robotsPolicy) allowsContext(ctx context.Context, target *url.URL) (bool, error) {
	if p.allowAll {
		return true, nil
	}
	if p.compiled == nil {
		return false, nil
	}
	return p.compiled.AllowsURLContext(ctx, target)
}

func (p robotsPolicy) estimatedMemoryBytes() int64 {
	if p.allowAll {
		return robotsAllowAllPolicyWeight + int64(len(p.robotsURL)+len(p.sitemaps)*32)
	}
	seen := map[*robotsparser.Policy]struct{}{}
	weight := int64(0)
	for _, compiled := range []*robotsparser.Policy{p.compiled, p.googlebot, p.oaiSearchBot, p.gptBot} {
		if compiled == nil {
			continue
		}
		if _, ok := seen[compiled]; ok {
			continue
		}
		seen[compiled] = struct{}{}
		weight += compiled.EstimatedMemoryBytes()
	}
	for _, compiled := range p.aiPolicies {
		if compiled == nil {
			continue
		}
		if _, ok := seen[compiled]; ok {
			continue
		}
		seen[compiled] = struct{}{}
		weight += compiled.EstimatedMemoryBytes()
	}
	for _, location := range p.sitemaps {
		weight += int64(len(location) + 32)
	}
	weight += int64(len(p.robotsURL) + len(p.aiPolicies)*48 + len(p.aiErrors)*48)
	for token, reason := range p.aiErrors {
		weight += int64(len(token) + len(reason))
	}
	return weight
}

type robotsPolicyCache struct {
	mu         sync.Mutex
	entries    map[string]*robotsCacheRecord
	ttl        time.Duration
	errorTTL   time.Duration
	maxEntries int
	maxWeight  int64
	weight     int64
	loadSlots  chan struct{}
}

type robotsCacheRecord struct {
	entry    *robotsCacheEntry
	lastUsed time.Time
	weight   int64
}

type robotsCacheEntry struct {
	mu        sync.Mutex
	policy    robotsPolicy
	err       error
	expiresAt time.Time
	loading   bool
	ready     chan struct{}
}

func newRobotsPolicyCache(ttl time.Duration, maxEntries int) *robotsPolicyCache {
	return newRobotsPolicyCacheWithLimits(
		ttl,
		maxEntries,
		DefaultRobotsCacheMaxWeight,
		DefaultRobotsLoadConcurrency,
	)
}

func newRobotsPolicyCacheWithLimits(
	ttl time.Duration,
	maxEntries int,
	maxWeight int64,
	maxConcurrentLoads int,
) *robotsPolicyCache {
	errorTTL := robotsErrorCacheTTL
	if ttl < errorTTL {
		errorTTL = ttl
	}
	if maxEntries <= 0 {
		maxEntries = 1
	}
	if maxWeight <= 0 {
		maxWeight = 1
	}
	if maxConcurrentLoads <= 0 {
		maxConcurrentLoads = 1
	}
	return &robotsPolicyCache{
		entries:    make(map[string]*robotsCacheRecord),
		ttl:        ttl,
		errorTTL:   errorTTL,
		maxEntries: maxEntries,
		maxWeight:  maxWeight,
		loadSlots:  make(chan struct{}, maxConcurrentLoads),
	}
}

func (c *robotsPolicyCache) isAllowedByRobots(
	ctx context.Context,
	client *http.Client,
	targetURL string,
	totalTimeout time.Duration,
) (bool, error) {
	allowed, _, err := c.inspectTarget(ctx, client, targetURL, totalTimeout)
	return allowed, err
}

func (c *robotsPolicyCache) inspectTarget(ctx context.Context, client *http.Client, targetURL string, totalTimeout time.Duration) (bool, geo.SearchControls, error) {
	search := geo.SearchControls{GooglebotRules: geo.Unknown, OAISearchBotRules: geo.Unknown, GPTBotRules: geo.Unknown, AIPolicies: unknownAIControls("robots.txt ще не перевірено")}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false, search, fmt.Errorf("parse target URL for robots.txt: %w", err)
	}

	key, err := robotsPolicyCacheKey(parsed)
	if err != nil {
		return false, search, err
	}
	robotsCtx, robotsCancel := context.WithTimeout(ctx, totalTimeout)
	defer robotsCancel()

	policy, err := c.policy(robotsCtx, key, func() (robotsPolicy, error) {
		return fetchRobotsPolicy(robotsCtx, client, parsed)
	})
	if err != nil {
		for i := range search.AIPolicies {
			control := &search.AIPolicies[i]
			control.Explanation = "Перевірка robots.txt не завершилася: " + redactText(sanitizeError(err))
			if policy.robotsURL != "" {
				control.RobotsURL = policy.robotsURL
			}
			if !policy.checkedAt.IsZero() {
				checkedAt := policy.checkedAt
				control.CheckedAt = &checkedAt
			}
			if policy.httpStatus != 0 {
				status := policy.httpStatus
				control.HTTPStatus = &status
			}
		}
		return false, search, err
	}
	allowed, err := policy.allowsContext(robotsCtx, parsed)
	if err != nil {
		return false, search, err
	}
	// Додаткові перевірки не змінюють рішення для власного User-Agent.
	inspectCtx, cancel := context.WithTimeout(robotsCtx, 250*time.Millisecond)
	defer cancel()
	search.AIPolicies = make([]geo.AIControl, 0, len(geo.SupportedAIPolicySpecs))
	for _, spec := range geo.SupportedAIPolicySpecs {
		control := geo.AIControl{
			Token: spec.Token, Provider: spec.Provider, Purpose: spec.Purpose,
			Status: geo.Unknown, RobotsURL: policy.robotsURL, Confidence: "unknown",
		}
		if !policy.checkedAt.IsZero() {
			checkedAt := policy.checkedAt
			control.CheckedAt = &checkedAt
		}
		if policy.httpStatus != 0 {
			status := policy.httpStatus
			control.HTTPStatus = &status
		}
		if policy.allowAll {
			control.Status = geo.Allowed
			control.Confidence = "robots_unavailable"
			control.Directive = "implicit allow: robots.txt returned an unavailable 4xx status"
			control.Explanation = "За RFC 9309 robots.txt із HTTP 4xx вважається недоступним; це не є підтвердженням доступу через WAF або мережу провайдера."
			search.AIPolicies = append(search.AIPolicies, control)
			continue
		}
		token := strings.ToLower(spec.Token)
		if reason := policy.aiErrors[token]; reason != "" {
			control.Explanation = "Правило не вдалося перевірити в межах ліміту: " + redactText(reason)
			search.AIPolicies = append(search.AIPolicies, control)
			continue
		}
		compiled := policy.aiPolicies[token]
		if compiled == nil {
			control.Explanation = "Немає достатніх даних для окремої перевірки цього токена."
			search.AIPolicies = append(search.AIPolicies, control)
			continue
		}
		decision, decisionErr := compiled.ExplainURLContext(inspectCtx, parsed)
		if decisionErr != nil {
			control.Explanation = "Перевірка зупинена лімітом часу або складності."
			search.AIPolicies = append(search.AIPolicies, control)
			continue
		}
		control.Status = geo.Allowed
		if !decision.Allowed {
			control.Status = geo.Blocked
		}
		control.Directive = safeRobotsDirective(parsed, decision.Directive)
		control.ConflictDirective = safeRobotsDirective(parsed, decision.ConflictDirective)
		control.Conflict = decision.Conflict
		control.Confidence = "robots_parsed"
		control.Explanation = "Статус відображає лише вибрану robots.txt директиву для цього URL."
		if decision.Conflict {
			control.Explanation = "Для URL збіглися Allow і Disallow однакової специфічності; за правилами robots перемагає Allow."
		}
		search.AIPolicies = append(search.AIPolicies, control)
	}
	search.GooglebotRules = aiControlStatus(search.AIPolicies, "Googlebot")
	search.OAISearchBotRules = aiControlStatus(search.AIPolicies, "OAI-SearchBot")
	search.GPTBotRules = aiControlStatus(search.AIPolicies, "GPTBot")
	return allowed, search, nil
}

func unknownAIControls(reason string) []geo.AIControl {
	controls := make([]geo.AIControl, 0, len(geo.SupportedAIPolicySpecs))
	for _, spec := range geo.SupportedAIPolicySpecs {
		controls = append(controls, geo.AIControl{Token: spec.Token, Provider: spec.Provider, Purpose: spec.Purpose, Status: geo.Unknown, Confidence: "unknown", Explanation: reason})
	}
	return controls
}

func aiControlStatus(controls []geo.AIControl, token string) string {
	for _, control := range controls {
		if strings.EqualFold(control.Token, token) {
			return control.Status
		}
	}
	return geo.Unknown
}

func robotsPolicyCacheKey(parsed *url.URL) (string, error) {
	authority, err := crawler.NormalizeAuthority(parsed)
	if err != nil {
		return "", fmt.Errorf("normalize robots.txt cache authority: %w", err)
	}
	return strings.ToLower(parsed.Scheme) + "://" + authority, nil
}

func (c *robotsPolicyCache) policy(
	ctx context.Context,
	key string,
	fetch func() (robotsPolicy, error),
) (robotsPolicy, error) {
	record := c.getRecord(key)
	entry := record.entry

	for {
		now := time.Now()
		entry.mu.Lock()
		if now.Before(entry.expiresAt) {
			policy, err := entry.policy, entry.err
			entry.mu.Unlock()
			return policy, err
		}
		if entry.loading {
			ready := entry.ready
			entry.mu.Unlock()
			select {
			case <-ctx.Done():
				return robotsPolicy{}, ctx.Err()
			case <-ready:
				continue
			}
		}

		entry.loading = true
		entry.ready = make(chan struct{})
		entry.mu.Unlock()

		policy, fetchErr := c.fetchWithinLoadBudget(ctx, fetch)
		policyWeight := int64(0)
		if fetchErr == nil {
			policyWeight = policy.estimatedMemoryBytes()
			if policyWeight > c.maxWeight {
				policy.googlebot, policy.oaiSearchBot, policy.gptBot = nil, nil, nil
				policy.aiPolicies, policy.aiErrors = nil, nil
				policyWeight = policy.estimatedMemoryBytes()
			}
			if policyWeight > c.maxWeight {
				policy.robotsURL, policy.checkedAt, policy.httpStatus = "", time.Time{}, 0
				policy.sitemaps = nil
				policyWeight = policy.estimatedMemoryBytes()
			}
			if policyWeight > c.maxWeight {
				fetchErr = fmt.Errorf(
					"compiled robots.txt policy requires an estimated %d bytes, cache budget is %d bytes",
					policyWeight,
					c.maxWeight,
				)
				policy = robotsPolicy{}
				policyWeight = 0
			}
		} else {
			policyWeight = policy.estimatedMemoryBytes()
			if policyWeight > c.maxWeight {
				policy = robotsPolicy{}
				policyWeight = 0
			}
		}
		now = time.Now()

		entry.mu.Lock()
		if fetchErr == nil {
			entry.policy = policy
			entry.err = nil
			entry.expiresAt = now.Add(c.ttl)
		} else {
			entry.policy = policy
			entry.err = fetchErr
			entry.expiresAt = now.Add(c.errorTTL)
		}
		c.setRecordWeight(key, record, policyWeight)
		entry.loading = false
		close(entry.ready)
		entry.mu.Unlock()
		return policy, fetchErr
	}
}

func (c *robotsPolicyCache) fetchWithinLoadBudget(
	ctx context.Context,
	fetch func() (robotsPolicy, error),
) (robotsPolicy, error) {
	select {
	case <-ctx.Done():
		return robotsPolicy{}, ctx.Err()
	case c.loadSlots <- struct{}{}:
	}
	defer func() { <-c.loadSlots }()
	return fetch()
}

func (c *robotsPolicyCache) getEntry(key string) *robotsCacheEntry {
	return c.getRecord(key).entry
}

func (c *robotsPolicyCache) getRecord(key string) *robotsCacheRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if record, ok := c.entries[key]; ok {
		record.lastUsed = now
		return record
	}

	if len(c.entries) >= c.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		for candidateKey, candidate := range c.entries {
			if oldestKey == "" || candidate.lastUsed.Before(oldestTime) {
				oldestKey = candidateKey
				oldestTime = candidate.lastUsed
			}
		}
		if oldestKey != "" {
			c.weight -= c.entries[oldestKey].weight
			delete(c.entries, oldestKey)
		}
	}

	entry := &robotsCacheEntry{}
	record := &robotsCacheRecord{entry: entry, lastUsed: now}
	c.entries[key] = record
	return record
}

func (c *robotsPolicyCache) setRecordWeight(key string, record *robotsCacheRecord, weight int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, ok := c.entries[key]
	if !ok || current != record {
		return
	}
	c.weight -= record.weight
	record.weight = weight
	c.weight += weight

	for c.weight > c.maxWeight {
		oldestKey := ""
		var oldestTime time.Time
		for candidateKey, candidate := range c.entries {
			if candidateKey == key || candidate.weight == 0 {
				continue
			}
			if oldestKey == "" || candidate.lastUsed.Before(oldestTime) {
				oldestKey = candidateKey
				oldestTime = candidate.lastUsed
			}
		}
		if oldestKey == "" {
			break
		}
		c.weight -= c.entries[oldestKey].weight
		delete(c.entries, oldestKey)
	}
}

func isAllowedByRobots(ctx context.Context, client *http.Client, targetURL string, timeout time.Duration) (bool, error) {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false, fmt.Errorf("parse target URL for robots.txt: %w", err)
	}
	robotsCtx, robotsCancel := context.WithTimeout(ctx, timeout)
	defer robotsCancel()

	policy, err := fetchRobotsPolicy(robotsCtx, client, parsed)
	if err != nil {
		return false, err
	}
	return policy.allowsContext(robotsCtx, parsed)
}

func fetchRobotsPolicy(
	ctx context.Context,
	client *http.Client,
	target *url.URL,
) (robotsPolicy, error) {
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", target.Scheme, target.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return robotsPolicy{}, fmt.Errorf("create robots.txt request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgentStr)

	attemptedAt := time.Now().UTC()
	resp, err := client.Do(req)
	if err != nil {
		return robotsPolicy{robotsURL: robotsURL, checkedAt: attemptedAt}, fmt.Errorf("fetch robots.txt from %s: %w", robotsURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
	case resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError:
		slog.Debug(
			"robots.txt недоступний, сканування дозволено відповідно до RFC 9309",
			"url",
			robotsURL,
			"status",
			resp.StatusCode,
		)
		return robotsPolicy{allowAll: true, robotsURL: robotsURL, checkedAt: attemptedAt, httpStatus: resp.StatusCode}, nil
	case resp.StatusCode >= http.StatusInternalServerError:
		return robotsPolicy{robotsURL: robotsURL, checkedAt: attemptedAt, httpStatus: resp.StatusCode}, fmt.Errorf("robots.txt is unreachable: %s returned HTTP %d", robotsURL, resp.StatusCode)
	case resp.StatusCode >= http.StatusMultipleChoices:
		return robotsPolicy{robotsURL: robotsURL, checkedAt: attemptedAt, httpStatus: resp.StatusCode}, fmt.Errorf(
			"robots.txt redirect chain did not resolve within %d redirects: final HTTP %d at %s",
			MaxRobotsRedirects,
			resp.StatusCode,
			resp.Request.URL,
		)
	default:
		return robotsPolicy{}, fmt.Errorf("robots.txt returned unexpected HTTP status %d from %s", resp.StatusCode, robotsURL)
	}

	body, err := readLimited(resp.Body, MaxRobotsBodyBytes)
	if err != nil {
		return robotsPolicy{robotsURL: robotsURL, checkedAt: attemptedAt, httpStatus: resp.StatusCode}, fmt.Errorf("read robots.txt from %s: %w", robotsURL, err)
	}
	compiled, err := robotsparser.CompilePolicyContext(ctx, string(body), UserAgentStr)
	if err != nil {
		return robotsPolicy{robotsURL: robotsURL, checkedAt: attemptedAt, httpStatus: resp.StatusCode}, fmt.Errorf("compile robots.txt from %s: %w", robotsURL, err)
	}
	policy := robotsPolicy{
		compiled:   compiled,
		sitemaps:   robotsSitemapLocations(string(body)),
		robotsURL:  robotsURL,
		checkedAt:  attemptedAt,
		httpStatus: resp.StatusCode,
	}
	// Лише додаткова GEO-діагностика: без нових HTTP-запитів та без підміни бота.
	inspectCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	tokens := make([]string, 0, len(geo.SupportedAIPolicySpecs))
	for _, spec := range geo.SupportedAIPolicySpecs {
		tokens = append(tokens, spec.Token)
	}
	policies, policyErrors, compileErr := robotsparser.CompilePoliciesContext(
		inspectCtx,
		string(body),
		tokens,
		maxAIBotPolicyRules,
		maxAIBotPolicyWeight,
	)
	policy.aiPolicies = make(map[string]*robotsparser.Policy, len(policies))
	policy.aiErrors = make(map[string]string, len(policyErrors))
	for token, compiled := range policies {
		policy.aiPolicies[strings.ToLower(token)] = compiled
	}
	for token, compileErr := range policyErrors {
		policy.aiErrors[strings.ToLower(token)] = sanitizeError(compileErr)
	}
	if compileErr != nil {
		for _, token := range tokens {
			if policy.aiPolicies[strings.ToLower(token)] == nil && policy.aiErrors[strings.ToLower(token)] == "" {
				policy.aiErrors[strings.ToLower(token)] = sanitizeError(compileErr)
			}
		}
	}
	policy.googlebot = policy.aiPolicies[strings.ToLower("Googlebot")]
	policy.oaiSearchBot = policy.aiPolicies[strings.ToLower("OAI-SearchBot")]
	policy.gptBot = policy.aiPolicies[strings.ToLower("GPTBot")]
	return policy, nil
}

func safeRobotsDirective(target *url.URL, directive string) string {
	if directive == "" || strings.HasPrefix(directive, "implicit allow") {
		return directive
	}
	name, pattern, ok := strings.Cut(directive, ": ")
	if !ok || target == nil {
		return directive
	}
	base := *target
	base.Path, base.RawPath, base.RawQuery, base.Fragment = "", "", "", ""
	base.User = nil
	base.Path = "/"
	joined := strings.TrimSuffix(base.String(), "/") + pattern
	redacted := redactURL(joined)
	parsed, err := url.Parse(redacted)
	if err != nil {
		return name + ": [redacted]"
	}
	path := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	if len(path) > 512 {
		path = path[:512]
	}
	return name + ": " + path
}
