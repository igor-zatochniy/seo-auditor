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
	maxRetries     int
	attemptTimeout time.Duration
	baseDelay      time.Duration
	maxDelay       time.Duration
}

type retryRoundTripper struct {
	base     http.RoundTripper
	policy   retryPolicy
	policies *hostPolicyManager
}

func (t *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	maxRetries := 0
	if isIdempotentRequest(req) {
		maxRetries = t.policy.maxRetries
	}

	for attempt := 0; ; attempt++ {
		var lease *hostLease
		if t.policies != nil {
			var err error
			lease, err = t.policies.acquire(req.Context(), req.URL)
			if err != nil {
				return nil, fmt.Errorf("acquire host request slot: %w", err)
			}
		}

		attemptCtx, attemptCancel := attemptContext(req.Context(), t.policy.attemptTimeout)
		attemptRequest, err := cloneRequestForRetry(req, attempt, attemptCtx)
		if err != nil {
			attemptCancel()
			releaseHostLease(lease)
			return nil, err
		}
		resp, requestErr := base.RoundTrip(attemptRequest)
		if t.policies != nil && resp != nil {
			t.policies.observeRetryAfter(req.URL, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
		retryable := shouldRetryHTTP(resp, requestErr) ||
			isRetryableAttemptTimeout(requestErr, req.Context(), attemptCtx)
		if !retryable || attempt >= maxRetries {
			if requestErr != nil {
				if resp != nil && resp.Body != nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
					_ = resp.Body.Close()
				}
				attemptCancel()
				releaseHostLease(lease)
				return resp, requestErr
			}
			if resp != nil && resp.Body != nil {
				responseBody := resp.Body
				if lease != nil {
					responseBody = &releaseOnCloseBody{ReadCloser: responseBody, release: lease.release}
				}
				resp.Body = &cancelOnCloseBody{
					ReadCloser: responseBody,
					cancel:     attemptCancel,
				}
			} else {
				attemptCancel()
				releaseHostLease(lease)
			}
			return resp, requestErr
		}

		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
		}
		attemptCancel()
		releaseHostLease(lease)

		delay := retryDelay(t.policy, attempt)
		if resp != nil {
			if deadline, ok := retryAfterDeadline(resp.Header.Get("Retry-After"), time.Now(), MaxRetryAfterDelay); ok {
				retryAfterDelay := time.Until(deadline)
				if retryAfterDelay > delay {
					delay = retryAfterDelay
				}
			}
		}
		if delay, err = clampRetryDelay(req.Context(), delay); err != nil {
			return nil, err
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

func releaseHostLease(lease *hostLease) {
	if lease != nil {
		lease.release()
	}
}

func cloneRequestForRetry(req *http.Request, attempt int, ctx context.Context) (*http.Request, error) {
	clone := req.Clone(ctx)
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

func attemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func isRetryableAttemptTimeout(err error, totalCtx context.Context, attemptCtx context.Context) bool {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && totalCtx.Err() == nil
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
	return retryDBOperationIf(ctx, operation, policy, isRetryableDBError, fn)
}

func retryDBMutation(ctx context.Context, operation string, policy retryPolicy, fn func() error) error {
	return retryDBOperationIf(ctx, operation, policy, isSafeToRetryDBMutationError, fn)
}

func retryDBOperationIf(
	ctx context.Context,
	operation string,
	policy retryPolicy,
	retryable func(error) bool,
	fn func() error,
) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt >= policy.maxRetries || !retryable(err) {
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

// Mutations are retried only when PostgreSQL confirms that the operation was
// not applied or the whole transaction was rolled back.
func isSafeToRetryDBMutationError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if pgconn.SafeToRetry(err) {
		return true
	}
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		return false
	}
	switch pgError.Code {
	case "40001", "40P01", "55P03":
		return true
	default:
		return false
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

func clampRetryDelay(ctx context.Context, delay time.Duration) (time.Duration, error) {
	if delay <= 0 {
		return 0, nil
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return delay, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, ctx.Err()
	}
	if delay > remaining {
		delay = remaining
	}
	return delay, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
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
