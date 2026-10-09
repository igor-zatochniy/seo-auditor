package geo

import (
	"fmt"
	"strings"
	"testing"
)

func visibilityInput(source, csv string) VisibilityInput {
	return VisibilityInput{Source: source, Engine: "chatgpt", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", Country: "US", Device: "all", CSV: csv}
}

func TestVisibilityImportPreservesScopeAndUnknownMetrics(t *testing.T) {
	for _, tc := range []struct {
		source, csv, scope string
		clicks             bool
	}{
		{"gsc_web", "\ufeffTop queries,Clicks,Impressions,CTR,Position\nchargebacks,38,1420,2%,4\n", "query", true},
		{"gsc_ai", "Page,Impressions\nhttps://example.com/page,1420\n", "url", false},
		{"ai_observed", "Query;URL;Impressions;Clicks\nchargebacks;https://example.com/page;;0\n", "query_url", true},
	} {
		in := visibilityInput(tc.source, tc.csv)
		rows, err := ParseVisibilityCSV(&in)
		if err != nil || len(rows) != 1 || rows[0].Scope != tc.scope || (rows[0].Clicks != nil) != tc.clicks {
			t.Fatalf("%s: %+v %v", tc.source, rows, err)
		}
	}
}

func TestVisibilityImportRejectsFabricatedOrInvalidData(t *testing.T) {
	for _, tc := range []struct{ source, csv string }{
		{"gsc_ai", "Query,Page,Impressions\nchargebacks,https://example.com/,4\n"},
		{"gsc_ai", "Page,Impressions,Clicks\nhttps://example.com/,4,1\n"},
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
