package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultMaxConcurrentPerHost  = 1
	MaxPerHostConcurrency        = 16
	DefaultHostStateCacheSize    = 4096
	DefaultRobotsCacheTTL        = time.Hour
	MaxRobotsCacheTTL            = 24 * time.Hour
	DefaultRobotsCacheMaxEntries = 4096
	MaxRetryAfterDelay           = 5 * time.Minute
)

type hostPolicyManager struct {
	mu            sync.Mutex
	states        map[string]*hostPolicyState
	interval      time.Duration
	maxConcurrent int
	maxEntries    int
	maxRetryAfter time.Duration
}

type hostPolicyState struct {
	limiter    *rate.Limiter
	slots      chan struct{}
	active     int
	lastUsed   time.Time
	retryUntil time.Time
}

type hostLease struct {
	manager *hostPolicyManager
	state   *hostPolicyState
	once    sync.Once
}

func newHostPolicyManager(
	interval time.Duration,
	maxConcurrent int,
	maxEntries int,
	maxRetryAfter time.Duration,
) *hostPolicyManager {
	return &hostPolicyManager{
		states:        make(map[string]*hostPolicyState),
		interval:      interval,
		maxConcurrent: maxConcurrent,
		maxEntries:    maxEntries,
		maxRetryAfter: maxRetryAfter,
	}
}

func (m *hostPolicyManager) acquire(ctx context.Context, target *url.URL) (*hostLease, error) {
	key, err := hostPolicyKey(target)
	if err != nil {
		return nil, err
	}

	state := m.retainState(key)
	lease := &hostLease{manager: m, state: state}

	select {
	case <-ctx.Done():
		m.releaseState(state)
		return nil, ctx.Err()
	case state.slots <- struct{}{}:
	}

	if err := m.waitForRetryAfter(ctx, state); err != nil {
		lease.release()
		return nil, err
	}
	if err := state.limiter.Wait(ctx); err != nil {
		lease.release()
		return nil, err
	}

	return lease, nil
}

func (m *hostPolicyManager) retainState(key string) *hostPolicyState {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.getOrCreateStateLocked(key)
	state.active++
	state.lastUsed = time.Now()
	return state
}

func (m *hostPolicyManager) getOrCreateStateLocked(key string) *hostPolicyState {
	if state, ok := m.states[key]; ok {
		return state
	}

	if len(m.states) >= m.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		for candidateKey, candidate := range m.states {
			if candidate.active != 0 {
				continue
			}
			if oldestKey == "" || candidate.lastUsed.Before(oldestTime) {
				oldestKey = candidateKey
				oldestTime = candidate.lastUsed
			}
		}
		if oldestKey != "" {
			delete(m.states, oldestKey)
		}
	}

	state := &hostPolicyState{
		limiter:  rate.NewLimiter(rate.Every(m.interval), 1),
		slots:    make(chan struct{}, m.maxConcurrent),
		lastUsed: time.Now(),
	}
	m.states[key] = state
	return state
}

func (m *hostPolicyManager) releaseState(state *hostPolicyState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.active > 0 {
		state.active--
	}
	state.lastUsed = time.Now()
}

func (m *hostPolicyManager) waitForRetryAfter(ctx context.Context, state *hostPolicyState) error {
	for {
		m.mu.Lock()
		retryUntil := state.retryUntil
		m.mu.Unlock()

		delay := time.Until(retryUntil)
		if delay <= 0 {
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *hostPolicyManager) observeRetryAfter(target *url.URL, statusCode int, value string) {
	if statusCode != http.StatusTooManyRequests && statusCode != http.StatusServiceUnavailable {
		return
	}
	deadline, ok := retryAfterDeadline(value, time.Now(), m.maxRetryAfter)
	if !ok {
		return
	}

	key, err := hostPolicyKey(target)
	if err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.getOrCreateStateLocked(key)
	if deadline.After(state.retryUntil) {
		state.retryUntil = deadline
	}
	state.lastUsed = time.Now()
}

func hostPolicyKey(target *url.URL) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" {
		return "", fmt.Errorf("cannot apply host policy to URL without host")
	}
	return host, nil
}

func retryAfterDeadline(value string, now time.Time, maximum time.Duration) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		maximumSeconds := int64(maximum / time.Second)
		if seconds > maximumSeconds {
			return now.Add(maximum), true
		}
		delay := time.Duration(seconds) * time.Second
		return now.Add(delay), true
	}

	parsed, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	delay := parsed.Sub(now)
	if delay <= 0 {
		return now, true
	}
	if delay > maximum {
		delay = maximum
	}
	return now.Add(delay), true
}

func (l *hostLease) release() {
	l.once.Do(func() {
		<-l.state.slots
		l.manager.releaseState(l.state)
	})
}

type politeRoundTripper struct {
	base     http.RoundTripper
	policies *hostPolicyManager
}

func (t *politeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	lease, err := t.policies.acquire(req.Context(), req.URL)
	if err != nil {
		return nil, fmt.Errorf("acquire host request slot: %w", err)
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		lease.release()
		return nil, err
	}

	t.policies.observeRetryAfter(req.URL, resp.StatusCode, resp.Header.Get("Retry-After"))
	if resp.Body == nil {
		lease.release()
		return resp, nil
	}
	resp.Body = &releaseOnCloseBody{ReadCloser: resp.Body, release: lease.release}
	return resp, nil
}

type releaseOnCloseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releaseOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
