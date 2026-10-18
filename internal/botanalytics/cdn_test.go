package botanalytics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCDNNativeFormatsAndUnknownOrigin(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		format, raw string
		origin      int
		cache       string
	}{
		{"cloudflare", `{"ClientIP":"203.0.113.3","ClientRequestScheme":"https","ClientRequestHost":"example.com","ClientRequestURI":"/p?token=a+b%2Fc","ClientRequestMethod":"GET","ClientRequestUserAgent":"OAI-SearchBot","EdgeResponseStatus":403,"OriginResponseStatus":200,"CacheCacheStatus":"HIT","RayID":"cf1","EdgeStartTimestamp":"2026-10-18T11:00:00Z"}`, 200, "hit"},
		{"vercel", `{"requestId":"v1","statusCode":500,"proxy":{"timestamp":1792321200000,"scheme":"https","host":"example.com","path":"/p?token=a+b%2Fc","method":"GET","userAgent":["OAI-SearchBot"],"clientIp":"203.0.113.3","statusCode":429,"vercelCache":"MISS"}}`, 0, "miss"},
		{"fastly", `{"url":"https://example.com/p?token=a+b%2Fc","client_ip":"203.0.113.3","user_agent":"OAI-SearchBot","method":"GET","status":503,"origin_status":0,"cache_status":"PASS","request_id":"f1","timestamp":"2026-10-18T11:00:00Z"}`, 0, "pass"},
		{"cloudfront", "#Version: 1.0\n#Fields: date time x-host-header cs(Host) cs-protocol cs-uri-stem cs-uri-query c-ip cs-method cs(User-Agent) sc-status x-edge-response-result-type x-edge-request-id\n2026-10-18\t11:00:00\texample.com\td123.cloudfront.net\thttps\t/p\ttoken=a+b%2Fc\t203.0.113.3\tGET\tOAI-SearchBot\t403\tError\tc1\n", 0, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			rows, stats, err := Parse(context.Background(), strings.NewReader(tt.raw), tt.format, "example.com", nil, now)
			if err != nil || len(rows) != 1 || stats.Accepted != 1 {
				t.Fatalf("%+v %+v %v", rows, stats, err)
			}
			r := rows[0]
			if r.URL != "https://example.com/p?token=a+b%2Fc" || r.CacheStatus != tt.cache || r.Verification != "unverified" {
				t.Fatalf("event=%+v", r)
			}
			if tt.origin == 0 && r.OriginStatus != nil || tt.origin != 0 && (r.OriginStatus == nil || *r.OriginStatus != tt.origin) {
				t.Fatalf("invented/missing origin %+v", r)
			}
		})
	}
}
func TestCDNBoundsDuplicatesAndCancellation(t *testing.T) {
	now := time.Now()
	line := fmt.Sprintf(`{"url":"https://example.com/p","user_agent":"GPTBot","client_ip":"203.0.113.2","method":"GET","status":429,"request_id":"id1","timestamp":%q}`, now.UTC().Format(time.RFC3339Nano))
	rows, stats, err := Parse(context.Background(), strings.NewReader(line+"\n"+line), "fastly", "example.com", nil, now)
	if err != nil || len(rows) != 1 || stats.Duplicates != 1 {
		t.Fatalf("%+v %+v %v", rows, stats, err)
	}
	_, _, err = Parse(context.Background(), strings.NewReader(line+"\n"+strings.Replace(line, `"status":429`, `"status":503`, 1)), "fastly", "example.com", nil, now)
	if err == nil {
		t.Fatal("conflicting request id accepted")
	}
	_, stats, err = Parse(context.Background(), strings.NewReader(strings.Replace(line, `"status":429`, `"status":0`, 1)), "fastly", "example.com", nil, now)
	if err != nil || stats.NoHTTP != 1 {
		t.Fatalf("000 classified HTTP failure %+v %v", stats, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = Parse(ctx, strings.NewReader(line), "fastly", "example.com", nil, now); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, _, err = Parse(context.Background(), strings.NewReader(strings.Repeat(line+"\n", MaxLines+1)), "fastly", "example.com", nil, now); err == nil {
		t.Fatal("silently truncated")
	}
}
