package botanalytics

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestJSONLDomainBotAndVerification(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	input := `{"time":"2026-10-13T09:00:00Z","remote_addr":"66.249.66.1","request":"GET /reset?token=fixture HTTP/1.1","user_agent":"Googlebot/2.1","status":200}` + "\n" +
		`{"time":"2026-10-13T09:00:01Z","remote_addr":"8.8.8.8","request_uri":"/missing","http_user_agent":"Googlebot/2.1","status":"404"}` + "\n" +
		`{"time":"2026-10-13T09:00:02Z","remote_addr":"8.8.8.8","request_uri":"https://other.example/","http_user_agent":"Googlebot/2.1","status":200}` + "\n" +
		`{"time":"2026-10-13T09:00:03Z","request_uri":"/","user_agent":"ordinary browser","status":200}`
	ranges := map[string]Ranges{"googlebot": {Prefixes: []netip.Prefix{netip.MustParsePrefix("66.249.66.0/24")}, FetchedAt: now}}
	records, stats, err := Parse(context.Background(), strings.NewReader(input), "jsonl", "example.com", ranges, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || stats.NonBots != 1 || stats.ForeignDomain != 1 || records[0].Verification != "matched_range" || records[1].Verification != "range_mismatch" {
		t.Fatalf("unexpected records: %+v %+v", records, stats)
	}
	if !strings.Contains(records[0].URL, "token=fixture") {
		t.Fatal("in-memory request identity was lost")
	}
}

func TestCombinedAndStaleRanges(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	line := `66.249.66.1 - - [13/Oct/2026:09:00:00 +0000] "GET /page HTTP/1.1" 503 20 "-" "Googlebot/2.1"`
	records, stats, err := Parse(context.Background(), strings.NewReader(line), "combined", "example.com", map[string]Ranges{"googlebot": {[]netip.Prefix{netip.MustParsePrefix("66.249.66.0/24")}, now.Add(-8 * 24 * time.Hour)}}, now)
	if err != nil || stats.Accepted != 1 || records[0].Verification != "unverified" {
		t.Fatalf("unexpected records: %+v %+v %v", records, stats, err)
	}
}

func TestLogBudgetsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Parse(ctx, strings.NewReader("{}"), "jsonl", "example.com", nil, time.Now()); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, _, err := Parse(context.Background(), strings.NewReader(strings.Repeat("a", 9000)), "jsonl", "example.com", nil, time.Now()); err == nil {
		t.Fatal("line limit ignored")
	}
	if _, _, err := Parse(context.Background(), strings.NewReader("{}"), "jsonl", "bad/domain", nil, time.Now()); err == nil {
		t.Fatal("invalid domain accepted")
	}
}

func TestPrefixValidationAndMappedIPv4(t *testing.T) {
	prefixes, err := ParseRanges([]byte(`{"prefixes":[{"ipv4Prefix":"66.249.66.0/24"},{"ipv6Prefix":"2001:4860:4801:10::/64"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if Verify(netip.MustParseAddr("::ffff:66.249.66.1"), Ranges{prefixes, now}, now) != "matched_range" {
		t.Fatal("mapped IPv4 not verified")
	}
	for _, raw := range []string{`{"prefixes":[{"ipv4Prefix":"0.0.0.0/0"}]}`, `{"prefixes":[{"ipv4Prefix":"127.0.0.0/8"}]}`, `{"prefixes":[]}`} {
		if _, err := ParseRanges([]byte(raw)); err == nil {
			t.Fatalf("unsafe prefix accepted: %s", raw)
		}
	}
}

func TestBotClaimBoundaries(t *testing.T) {
	for _, ua := range []string{"Googlebot", "Googlebot/2.1", "Googlebot-Image/1.0", "Mozilla/5.0 (compatible; Googlebot/2.1;)"} {
		if Classify(ua) != "googlebot" {
			t.Fatalf("bot not recognized: %s", ua)
		}
	}
	for _, ua := range []string{"NotGooglebot/2.1", "Custom-GPTBot/1.0", "ordinary browser"} {
		if Classify(ua) != "" {
			t.Fatalf("substring misclassified: %s", ua)
		}
	}
}
