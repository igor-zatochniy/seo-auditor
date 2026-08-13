package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRetryRoundTripperRetriesTransientHTTPStatus(t *testing.T) {
	var attempts atomic.Int32
	transport := &retryRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			status := http.StatusOK
			if attempts.Add(1) == 1 {
				status = http.StatusServiceUnavailable
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("response")),
				Request:    req,
			}, nil
		}),
		policy: retryPolicy{maxRetries: 2, attemptTimeout: time.Second, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || attempts.Load() != 2 {
		t.Fatalf("unexpected retry result: status=%d attempts=%d", resp.StatusCode, attempts.Load())
	}
}

func TestRetryRoundTripperRetriesConnectionReset(t *testing.T) {
	var attempts atomic.Int32
	transport := &retryRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, syscall.ECONNRESET
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		policy: retryPolicy{maxRetries: 1, attemptTimeout: time.Second, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()
	if attempts.Load() != 2 {
		t.Fatalf("unexpected attempt count: %d", attempts.Load())
	}
}

func TestRetryDBOperationRetriesSerializationFailure(t *testing.T) {
	var attempts int
	err := retryDBOperation(
		context.Background(),
		"test_operation",
		retryPolicy{maxRetries: 2, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
		func() error {
			attempts++
			if attempts == 1 {
				return &pgconn.PgError{Code: "40001", Message: "serialization failure"}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("retryDBOperation returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("unexpected attempt count: %d", attempts)
	}
}

func TestRetryDBOperationDoesNotRetryContextCancellation(t *testing.T) {
	attempts := 0
	err := retryDBOperation(
		context.Background(),
		"test_operation",
		retryPolicy{maxRetries: 2, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
		func() error {
			attempts++
			return context.Canceled
		},
	)
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("unexpected cancellation result: error=%v attempts=%d", err, attempts)
	}
}

func TestRetryDBMutationRetriesRolledBackTransaction(t *testing.T) {
	attempts := 0
	err := retryDBMutation(
		context.Background(),
		"test_mutation",
		retryPolicy{maxRetries: 2, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
		func() error {
			attempts++
			if attempts == 1 {
				return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("retryDBMutation returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("unexpected attempt count: %d", attempts)
	}
}

func TestRetryDBMutationDoesNotRetryAmbiguousNetworkFailure(t *testing.T) {
	attempts := 0
	err := retryDBMutation(
		context.Background(),
		"test_mutation",
		retryPolicy{maxRetries: 2, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
		func() error {
			attempts++
			return syscall.ECONNRESET
		},
	)
	if !errors.Is(err, syscall.ECONNRESET) || attempts != 1 {
		t.Fatalf("unexpected mutation retry result: error=%v attempts=%d", err, attempts)
	}
}

func TestRetryRoundTripperUsesAttemptTimeout(t *testing.T) {
	var attempts atomic.Int32
	transport := &retryRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		policy: retryPolicy{maxRetries: 1, attemptTimeout: 20 * time.Millisecond, baseDelay: time.Nanosecond, maxDelay: time.Microsecond},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if attempts.Load() != 2 {
		t.Fatalf("unexpected attempt count: %d", attempts.Load())
	}
}

func TestRetryRoundTripperStartsAttemptTimeoutAfterPolitenessWait(t *testing.T) {
	const rateInterval = 80 * time.Millisecond
	const attemptTimeout = 20 * time.Millisecond

	var networkAttempts atomic.Int32
	manager := newHostPolicyManager(rateInterval, 1, 16, time.Second)
	transport := &retryRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			networkAttempts.Add(1)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			default:
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		policies: manager,
		policy: retryPolicy{
			attemptTimeout: attemptTimeout,
		},
	}
	client := &http.Client{Transport: transport}

	first, err := client.Get("https://example.com/first")
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = first.Body.Close()

	started := time.Now()
	requestCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://example.com/second", nil)
	if err != nil {
		t.Fatalf("create second request: %v", err)
	}
	second, err := client.Do(request)
	if err != nil {
		t.Fatalf("second request failed after politeness wait: %v", err)
	}
	_ = second.Body.Close()

	if elapsed := time.Since(started); elapsed < rateInterval-attemptTimeout {
		t.Fatalf("politeness interval was not applied: elapsed=%s", elapsed)
	}
	if networkAttempts.Load() != 2 {
		t.Fatalf("network attempts = %d, want 2", networkAttempts.Load())
	}
}

func TestRetryRoundTripperHoldsHostSlotUntilResponseBodyCloses(t *testing.T) {
	manager := newHostPolicyManager(time.Nanosecond, 1, 16, time.Second)
	transport := &retryRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		policies: manager,
		policy: retryPolicy{
			attemptTimeout: time.Second,
		},
	}
	client := &http.Client{Transport: transport}

	first, err := client.Get("https://example.com/first")
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	requestCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://example.com/second", nil)
	if err != nil {
		t.Fatalf("create second request: %v", err)
	}
	if _, err := client.Do(request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second request error = %v, want context deadline exceeded while first body is open", err)
	}

	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first response body: %v", err)
	}
	third, err := client.Get("https://example.com/third")
	if err != nil {
		t.Fatalf("third request failed after releasing host slot: %v", err)
	}
	_ = third.Body.Close()
}

func TestClampRetryDelayToContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	delay, err := clampRetryDelay(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("clampRetryDelay returned error: %v", err)
	}
	if delay <= 0 || delay > time.Second {
		t.Fatalf("unexpected clamped delay: %s", delay)
	}
}
