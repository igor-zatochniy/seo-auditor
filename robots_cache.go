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
)

const robotsErrorCacheTTL = time.Minute

type robotsPolicy struct {
	allowAll bool
	content  string
}

func (p robotsPolicy) allows(target *url.URL) bool {
	if p.allowAll {
		return true
	}
	return isPathAllowedByRobots(p.content, UserAgentStr, robotsRequestPath(target))
}

type robotsPolicyCache struct {
	mu         sync.Mutex
	entries    map[string]*robotsCacheRecord
	ttl        time.Duration
	errorTTL   time.Duration
	maxEntries int
}

type robotsCacheRecord struct {
	entry    *robotsCacheEntry
	lastUsed time.Time
}

type robotsCacheEntry struct {
	mu        sync.Mutex
	policy    robotsPolicy
	hasPolicy bool
	err       error
	expiresAt time.Time
	loading   bool
	ready     chan struct{}
}

func newRobotsPolicyCache(ttl time.Duration, maxEntries int) *robotsPolicyCache {
	errorTTL := robotsErrorCacheTTL
	if ttl < errorTTL {
		errorTTL = ttl
	}
	return &robotsPolicyCache{
		entries:    make(map[string]*robotsCacheRecord),
		ttl:        ttl,
		errorTTL:   errorTTL,
		maxEntries: maxEntries,
	}
}

func (c *robotsPolicyCache) isAllowedByRobots(
	ctx context.Context,
	client *http.Client,
	targetURL string,
	timeout time.Duration,
) (bool, error) {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false, fmt.Errorf("parse target URL for robots.txt: %w", err)
	}

	key := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	policy, err := c.policy(ctx, key, func() (robotsPolicy, error) {
		return fetchRobotsPolicy(ctx, client, parsed, timeout)
	})
	if err != nil {
		return false, err
	}
	return policy.allows(parsed), nil
}

func (c *robotsPolicyCache) policy(
	ctx context.Context,
	key string,
	fetch func() (robotsPolicy, error),
) (robotsPolicy, error) {
	entry := c.getEntry(key)

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

		stalePolicy := entry.policy
		hasStalePolicy := entry.hasPolicy
		entry.loading = true
		entry.ready = make(chan struct{})
		entry.mu.Unlock()

		policy, fetchErr := fetch()
		now = time.Now()

		entry.mu.Lock()
		if fetchErr == nil {
			entry.policy = policy
			entry.hasPolicy = true
			entry.err = nil
			entry.expiresAt = now.Add(c.ttl)
		} else if hasStalePolicy {
			slog.Warn("Оновлення robots.txt не вдалося, використовується кешована policy", "host", key, "error", fetchErr)
			entry.policy = stalePolicy
			entry.hasPolicy = true
			entry.err = nil
			entry.expiresAt = now.Add(c.errorTTL)
			policy = stalePolicy
			fetchErr = nil
		} else {
			entry.policy = robotsPolicy{}
			entry.hasPolicy = false
			entry.err = fetchErr
			entry.expiresAt = now.Add(c.errorTTL)
		}
		entry.loading = false
		close(entry.ready)
		entry.mu.Unlock()
		return policy, fetchErr
	}
}

func (c *robotsPolicyCache) getEntry(key string) *robotsCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if record, ok := c.entries[key]; ok {
		record.lastUsed = now
		return record.entry
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
			delete(c.entries, oldestKey)
		}
	}

	entry := &robotsCacheEntry{}
	c.entries[key] = &robotsCacheRecord{entry: entry, lastUsed: now}
	return entry
}

func isAllowedByRobots(ctx context.Context, client *http.Client, targetURL string, timeout time.Duration) (bool, error) {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false, fmt.Errorf("parse target URL for robots.txt: %w", err)
	}
	policy, err := fetchRobotsPolicy(ctx, client, parsed, timeout)
	if err != nil {
		return false, err
	}
	return policy.allows(parsed), nil
}

func fetchRobotsPolicy(
	ctx context.Context,
	client *http.Client,
	target *url.URL,
	timeout time.Duration,
) (robotsPolicy, error) {
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", target.Scheme, target.Host)

	robotsCtx, robotsCancel := context.WithTimeout(ctx, timeout)
	defer robotsCancel()

	req, err := http.NewRequestWithContext(robotsCtx, http.MethodGet, robotsURL, nil)
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
	return robotsPolicy{content: string(body)}, nil
}
