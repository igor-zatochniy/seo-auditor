package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoliteTransportLimitsConcurrencyPerHost(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		number := requests.Add(1)
		if number == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		if number == 2 {
			close(secondStarted)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		server.Close()
	})

	manager := newHostPolicyManager(time.Millisecond, 1, 16, 100*time.Millisecond)
	client := &http.Client{Transport: &politeRoundTripper{base: server.Client().Transport, policies: manager}}
	errors := make(chan error, 2)

	request := func() {
		resp, err := client.Get(server.URL)
		if err == nil {
			err = resp.Body.Close()
		}
		errors <- err
	}
	go request()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	go request()

	select {
	case <-secondStarted:
		t.Fatal("second request started before the first released its host slot")
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	for range 2 {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not complete")
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("unexpected request count: %d", requests.Load())
	}
}

func TestPoliteTransportHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := newHostPolicyManager(time.Millisecond, 1, 16, 100*time.Millisecond)
	client := &http.Client{Transport: &politeRoundTripper{base: server.Client().Transport, policies: manager}}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()

	started := time.Now()
	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_ = resp.Body.Close()
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
		t.Fatalf("Retry-After was not honored: elapsed=%s", elapsed)
	}
}

func TestPoliteTransportAppliesRateLimitPerHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := newHostPolicyManager(50*time.Millisecond, 1, 16, time.Second)
	client := &http.Client{Transport: &politeRoundTripper{base: server.Client().Transport, policies: manager}}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = resp.Body.Close()

	started := time.Now()
	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_ = resp.Body.Close()
	if elapsed := time.Since(started); elapsed < 35*time.Millisecond {
		t.Fatalf("per-host rate limit was not applied: elapsed=%s", elapsed)
	}
}
