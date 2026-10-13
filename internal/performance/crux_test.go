package performance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cruxTransport func(*http.Request) (*http.Response, error)

func (f cruxTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const cruxFixture = `{"record":{"key":{"url":"https://example.com/page","formFactor":"PHONE"},"metrics":{"largest_contentful_paint":{"percentiles":{"p75":2400}},"interaction_to_next_paint":{"percentiles":{"p75":210}},"cumulative_layout_shift":{"percentiles":{"p75":"0.08"}}},"collectionPeriod":{"firstDate":{"year":2026,"month":9,"day":15},"lastDate":{"year":2026,"month":10,"day":12}}}}`

func TestCrUXFieldMetricsAndSecretHeader(t *testing.T) {
	client := &http.Client{Transport: cruxTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != CrUXEndpoint || r.URL.RawQuery != "" || r.Header.Get("X-Goog-Api-Key") != "fixture-key-not-secret" {
			t.Fatal("key transport incorrect")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"url":"https://example.com/page"`) {
			t.Fatal("scope missing")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(cruxFixture))}, nil
	})}
	r, err := QueryCrUX(context.Background(), client, "fixture-key-not-secret", "https://example.com/page", "PHONE", "url")
	if err != nil || r.Status != "complete" || r.Metrics["largest_contentful_paint"].Rating != "good" || r.Metrics["interaction_to_next_paint"].Rating != "needs_improvement" || r.Metrics["cumulative_layout_shift"].P75 != 0.08 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

func TestCrUXMissingDataAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		state  string
		bad    bool
	}{{404, "", "no_data", false}, {429, "", "", true}, {200, strings.Replace(cruxFixture, `"0.08"`, `"NaN"`, 1), "partial", false}, {200, strings.Replace(cruxFixture, "example.com/page", "other.example/page", 1), "", true}, {200, strings.Repeat("x", 65537), "", true}, {200, strings.Replace(cruxFixture, `"day":12`, `"day":45`, 1), "", true}} {
		client := &http.Client{Transport: cruxTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}
		r, err := QueryCrUX(context.Background(), client, "fixture", "https://example.com/page", "PHONE", "url")
		if (err != nil) != tc.bad || !tc.bad && r.Status != tc.state {
			t.Fatalf("status=%d report=%+v err=%v", tc.status, r, err)
		}
	}
	for _, address := range []string{"http://127.0.0.1/", "https://example.com/?token=fixture", "https://user:pass@example.com/"} {
		if _, err := QueryCrUX(context.Background(), &http.Client{}, "fixture", address, "PHONE", "url"); err == nil {
			t.Fatal("unsafe address accepted")
		}
	}
}
