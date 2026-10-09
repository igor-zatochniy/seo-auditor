package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPObservationDoesNotTreatMissingTraceAsZero(t *testing.T) {
	_, read := observeHTTP(context.Background())
	if got := read(200); got.ResponseMS != nil || got.Slow {
		t.Fatalf("missing trace became measurement: %+v", got)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(30 * time.Millisecond); w.WriteHeader(200) }))
	defer server.Close()
	ctx, read := observeHTTP(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := read(resp.StatusCode)
	if got.ResponseMS == nil || *got.ResponseMS < 20 || got.Status != 200 {
		t.Fatalf("missing response timing: %+v", got)
	}
}
