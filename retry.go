package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type retryPolicy struct {
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
}

type retryRoundTripper struct {
	base   http.RoundTripper
	policy retryPolicy
}

func (t *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if !isIdempotentRequest(req) || t.policy.maxRetries == 0 {
		return base.RoundTrip(req)
	}

	for attempt := 0; ; attempt++ {
		attemptRequest, err := cloneRequestForRetry(req, attempt)
		if err != nil {
			return nil, err
		}
		resp, requestErr := base.RoundTrip(attemptRequest)
		if !shouldRetryHTTP(resp, requestErr) || attempt >= t.policy.maxRetries {
			return resp, requestErr
		}

		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
		}

		delay := retryDelay(t.policy, attempt)
		if resp != nil {
			if deadline, ok := retryAfterDeadline(resp.Header.Get("Retry-After"), time.Now(), MaxRetryAfterDelay); ok {
				retryAfterDelay := time.Until(deadline)
				if retryAfterDelay > delay {
					delay = retryAfterDelay
				}
			}
		}
		slog.Warn(
			"Повторюється тимчасово невдалий HTTP-запит",
			"url", redactURL(req.URL.String()),
			"attempt", attempt+1,
			"delay", delay.String(),
		)
		if err := waitForRetry(req.Context(), delay); err != nil {
			return nil, err
		}
	}
}

func cloneRequestForRetry(req *http.Request, attempt int) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if attempt == 0 || req.Body == nil {
		return clone, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("recreate request body: %w", err)
	}
	clone.Body = body
	return clone, nil
}

func isIdempotentRequest(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return req.Body == nil || req.GetBody != nil
	default:
		return false
	}
}

func shouldRetryHTTP(resp *http.Response, err error) bool {
	if err != nil {
		return isRetryableNetworkError(err)
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return dnsError.IsTimeout || !dnsError.IsNotFound
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT)
}

func retryDBOperation(ctx context.Context, operation string, policy retryPolicy, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt >= policy.maxRetries || !isRetryableDBError(err) {
			return err
		}
		delay := retryDelay(policy, attempt)
		slog.Warn(
			"Повторюється тимчасово невдала операція PostgreSQL",
			"operation", operation,
			"attempt", attempt+1,
			"delay", delay.String(),
			"error", err,
		)
		if err := waitForRetry(ctx, delay); err != nil {
			return err
		}
	}
}

func isRetryableDBError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if pgconn.SafeToRetry(err) || isRetryableNetworkError(err) {
		return true
	}
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		return false
	}
	if len(pgError.Code) >= 2 && pgError.Code[:2] == "08" {
		return true
	}
	switch pgError.Code {
	case "40001", "40P01", "55P03", "57P01", "57P02", "57P03":
		return true
	default:
		return false
	}
}

func retryDelay(policy retryPolicy, attempt int) time.Duration {
	delay := policy.baseDelay
	for i := 0; i < attempt && delay < policy.maxDelay; i++ {
		if delay > policy.maxDelay/2 {
			delay = policy.maxDelay
			break
		}
		delay *= 2
	}
	if delay > policy.maxDelay {
		delay = policy.maxDelay
	}
	if delay <= 0 {
		return 0
	}
	// Full jitter prevents synchronized retries across workers.
	return time.Duration(rand.Float64() * float64(delay))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
