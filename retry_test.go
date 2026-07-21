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
