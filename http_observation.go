package main

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

// Власний rate limit, DNS, TLS та backoff не є затримкою відповіді сервера.
func observeHTTP(ctx context.Context) (context.Context, func(int) *geo.HTTPObservation) {
	var mu sync.Mutex
	var written time.Time
	var elapsed *int64
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			written = time.Time{}
			elapsed = nil
			if info.Err == nil {
				written = time.Now()
			}
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			defer mu.Unlock()
			if !written.IsZero() {
				v := time.Since(written).Milliseconds()
				elapsed = &v
			}
		},
	}
	return httptrace.WithClientTrace(ctx, trace), func(status int) *geo.HTTPObservation {
		mu.Lock()
		defer mu.Unlock()
		r := &geo.HTTPObservation{Status: status}
		if elapsed != nil {
			v := *elapsed
			r.ResponseMS = &v
			r.Slow = v > 1000
		}
		return r
	}
}
