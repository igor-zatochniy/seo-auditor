package geo

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func visibilityInput(source, csv string) VisibilityInput {
	return VisibilityInput{Source: source, Engine: "chatgpt", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", Country: "US", Device: "all", CSV: csv}
}

func TestNormalizeDesiredAIPolicyDefaultsAndValidatesTokens(t *testing.T) {
	policy, err := NormalizeDesiredAIPolicy(map[string]string{"Google-Extended": Blocked, "oai-searchbot": Allowed})
	if err != nil {
		t.Fatalf("NormalizeDesiredAIPolicy() error = %v", err)
	}
	if len(policy) != len(SupportedAIPolicySpecs) || policy["google-extended"] != Blocked || policy["oai-searchbot"] != Allowed || policy["gptbot"] != Unknown {
		t.Fatalf("unexpected normalized policy: %+v", policy)
	}
	if _, err := NormalizeDesiredAIPolicy(map[string]string{"unknownbot": Allowed}); err == nil {
		t.Fatal("unsupported token was accepted")
	}
	if _, err := NormalizeDesiredAIPolicy(map[string]string{"gptbot": "maybe"}); err == nil {
		t.Fatal("unsupported policy state was accepted")
	}
}

func TestVisibilityImportPreservesScopeAndUnknownMetrics(t *testing.T) {
	for _, tc := range []struct {
		source, csv, scope                          string
		clicks, ctr, position, sessions, aiReferral bool
	}{
		{"gsc_web", "\ufeffTop queries,Clicks,Impressions,CTR,Position\nchargebacks,38,1420,2%,4\n", "query", true, true, true, false, false},
		{"gsc_ai", "Page,Impressions\nhttps://example.com/page,1420\n", "url", false, false, false, false, false},
		{"ga4", "Landing page,Session source,Sessions\n/pricing?utm_id=1,chatgpt.com / referral,12\n", "url", false, false, false, true, true},
		{"ai_observed", "Query;URL;Impressions;Clicks\nchargebacks;https://example.com/page;;0\n", "query_url", true, false, false, false, false},
	} {
		in := visibilityInput(tc.source, tc.csv)
		rows, err := ParseVisibilityCSV(&in)
		if err != nil || len(rows) != 1 || rows[0].Scope != tc.scope || (rows[0].Clicks != nil) != tc.clicks || (rows[0].CTR != nil) != tc.ctr || (rows[0].Position != nil) != tc.position || (rows[0].Sessions != nil) != tc.sessions || rows[0].AIReferral != tc.aiReferral {
			t.Fatalf("%s: %+v %v", tc.source, rows, err)
		}
		if tc.source == "gsc_web" && (math.Abs(*rows[0].CTR-.02) > 1e-9 || *rows[0].Position != 4) {
			t.Fatalf("GSC rates were not preserved: %+v", rows[0])
		}
	}
}

func TestVisibilityImportRejectsFabricatedOrInvalidData(t *testing.T) {
	for _, tc := range []struct{ source, csv string }{
		{"gsc_ai", "Query,Page,Impressions\nchargebacks,https://example.com/,4\n"},
		{"gsc_ai", "Page,Impressions,Clicks\nhttps://example.com/,4,1\n"},
		{"gsc_ai", "Page,Impressions,CTR\nhttps://example.com/,4,20%\n"},
		{"ga4", "Landing page,Sessions\n/pricing,12\n"},
		{"ga4", "Landing page,Session source,Sessions,Impressions\n/pricing,chatgpt.com,12,10\n"},
		{"gsc_web", "Query,Clicks,Impressions\nquery,5,4\n"},
		{"gsc_web", "Query,Impressions\nquery,-1\n"},
		{"gsc_web", "Query,Impressions\nquery,1.5\n"},
		{"gsc_web", "Query,Impressions\nquery,1\nQUERY,1\n"},
		{"gsc_web", "Query,Impressions,Date\nquery,1,2026-09-01\n"},
		{"gsc_web", "Query,Impressions,Impressions\nquery,1,1\n"},
		{"gsc_web", "Query,Impressions\nquery,\n"},
	} {
		in := visibilityInput(tc.source, tc.csv)
		if _, err := ParseVisibilityCSV(&in); err == nil {
			t.Fatalf("Прийнято хибні дані: %s", tc.csv)
		}
	}
	var b strings.Builder
	b.WriteString("Query,Impressions\n")
	for i := range MaxVisibilityRows + 1 {
		fmt.Fprintf(&b, "query %d,1\n", i)
	}
	in := visibilityInput("gsc_web", b.String())
	if _, err := ParseVisibilityCSV(&in); err == nil {
		t.Fatal("Необмежений імпорт")
	}
}

func TestAIReferralSourceUsesHostBoundaries(t *testing.T) {
	for _, source := range []struct {
		value string
		want  bool
	}{
		{"chatgpt.com / referral", true},
		{"chatgpt.com/referral", true},
		{"sub.perplexity.ai", true},
		{"gemini.google.com", true},
		{"notchatgpt.com", false},
		{"example.com", false},
	} {
		if got := IsAIReferralSource(source.value); got != source.want {
			t.Errorf("IsAIReferralSource(%q)=%v, want %v", source.value, got, source.want)
		}
	}
}
