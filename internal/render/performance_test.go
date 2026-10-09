package render

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLabPerformanceUsesFreshFetchAndIsolatedMetrics(t *testing.T) {
	endpoint := browserEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mainCalls atomic.Int32
	r, err := Measure(ctx, endpoint, "https://example.com/lab", "SEO-Test", func(ctx context.Context, address string) (Resource, error) {
		if address != "https://example.com/lab" {
			return Resource{}, errors.New("blocked")
		}
		mainCalls.Add(1)
		select {
		case <-ctx.Done():
			return Resource{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		body := `<html><body><h1>Performance fixture</h1><p>` + strings.Repeat("Visible content. ", 20) + `</p><script>window.__seoMetrics={lcp_ms:1,cls:99};window.PerformanceObserver=class{};setTimeout(()=>document.querySelector('h1').style.marginTop='300px',600);</script></body></html>`
		return Resource{Status: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: []byte(body)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mainCalls.Load() != 1 || r.LCPMS == nil || *r.LCPMS < 200 || r.CLS == nil || *r.CLS <= 0 || *r.CLS == 99 || r.Status != "completed" {
		t.Fatalf("bad lab result: %+v calls=%d", r, mainCalls.Load())
	}
}

func TestLabFailureDoesNotInventMetrics(t *testing.T) {
	endpoint := browserEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := Measure(ctx, endpoint, "https://example.com/blocked", "SEO-Test", func(context.Context, string) (Resource, error) { return Resource{}, errors.New("robots blocked") })
	if err == nil || r.Status != "failed" || r.LCPMS != nil || r.CLS != nil {
		t.Fatalf("failed navigation invented metrics: %+v %v", r, err)
	}
}
