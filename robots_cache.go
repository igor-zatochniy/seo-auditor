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
	robotsparser "github.com/igor-zatochniy/seo-auditor/internal/robots"
)

const (
	robotsErrorCacheTTL        = time.Minute
	robotsAllowAllPolicyWeight = int64(256)
	MaxRobotsBodyBytes         = int64(robotsparser.MaxPolicyBytes)
)

type robotsPolicy struct {
	allowAll bool
	compiled *robotsparser.Policy
}

func (p robotsPolicy) allows(target *url.URL) bool {
	if p.allowAll {
		return true
	}
	return p.compiled != nil && p.compiled.AllowsURL(target)
}

func (p robotsPolicy) estimatedMemoryBytes() int64 {
	if p.allowAll {
		return robotsAllowAllPolicyWeight
	}
	if p.compiled == nil {
		return 0
	}
	return p.compiled.EstimatedMemoryBytes()
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
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false, fmt.Errorf("parse target URL for robots.txt: %w", err)
	}

	key, err := robotsPolicyCacheKey(parsed)
	if err != nil {
		return false, err
	}
	robotsCtx, robotsCancel := context.WithTimeout(ctx, totalTimeout)
	defer robotsCancel()

	policy, err := c.policy(robotsCtx, key, func() (robotsPolicy, error) {
		return fetchRobotsPolicy(robotsCtx, client, parsed)
	})
	if err != nil {
		return false, err
	}
	return policy.allows(parsed), nil
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
				fetchErr = fmt.Errorf(
					"compiled robots.txt policy requires an estimated %d bytes, cache budget is %d bytes",
					policyWeight,
					c.maxWeight,
				)
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
			entry.policy = robotsPolicy{}
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
	return policy.allows(parsed), nil
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

	resp, err := client.Do(req)
	if err != nil {
		return robotsPolicy{}, fmt.Errorf("fetch robots.txt from %s: %w", robotsURL, err)
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
		return robotsPolicy{allowAll: true}, nil
	case resp.StatusCode >= http.StatusInternalServerError:
		return robotsPolicy{}, fmt.Errorf("robots.txt is unreachable: %s returned HTTP %d", robotsURL, resp.StatusCode)
	case resp.StatusCode >= http.StatusMultipleChoices:
		return robotsPolicy{}, fmt.Errorf(
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
		return robotsPolicy{}, fmt.Errorf("read robots.txt from %s: %w", robotsURL, err)
	}
	compiled, err := robotsparser.CompilePolicyContext(ctx, string(body), UserAgentStr)
	if err != nil {
		return robotsPolicy{}, fmt.Errorf("compile robots.txt from %s: %w", robotsURL, err)
	}
	return robotsPolicy{compiled: compiled}, nil
}
