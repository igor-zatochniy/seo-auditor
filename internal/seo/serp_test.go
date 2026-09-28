package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSERPWidthThresholds(t *testing.T) {
	for _, tt := range []struct {
		width int
		want  string
	}{
		{580, "Recommended"}, {581, "Borderline"}, {600, "Borderline"}, {601, "High truncation risk"},
	} {
		if got := titleWidthStatus(1, &tt.width); got != tt.want {
			t.Errorf("title %d: %s", tt.width, got)
		}
	}
	for _, tt := range []struct {
		width           int
		desktop, mobile string
	}{
		{680, "Safe", "Safe"}, {681, "Safe", "May truncate"}, {920, "Safe", "May truncate"}, {921, "May truncate", "Likely truncate"},
	} {
		desktop, mobile := descriptionWidthStatus(1, &tt.width)
		if desktop != tt.desktop || mobile != tt.mobile {
			t.Errorf("description %d: %s / %s", tt.width, desktop, mobile)
		}
	}
	zero := 0
	if titleWidthStatus(0, &zero) != "Missing" || titleWidthStatus(1, nil) != "Not measured" {
		t.Fatal("missing/unmeasured title treated as safe")
	}
	for _, chars := range []int{0, 1} {
		want := "Missing"
		if chars > 0 {
			want = "Not measured"
		}
		desktop, mobile := descriptionWidthStatus(chars, nil)
		if desktop != want || mobile != want {
			t.Fatal("missing/unmeasured description treated as safe")
		}
	}
}

func TestSERPWidthUsesProportionalFont(t *testing.T) {
	if serpFontError != nil {
		t.Fatal(serpFontError)
	}
	narrow, wide := serpWidthCounter{pixels: 14}, serpWidthCounter{pixels: 14}
	narrow.Write([]byte(strings.Repeat("i", 165)))
	wide.Write([]byte(strings.Repeat("W", 165)))
	if *narrow.Width() >= 680 || *wide.Width() <= 920 {
		t.Fatalf("equal character counts have unexpected widths: %d / %d", *narrow.Width(), *wide.Width())
	}
	whole, chunks := serpWidthCounter{pixels: 20}, serpWidthCounter{pixels: 20}
	whole.Write([]byte("  AV\n  Україна  "))
	for _, chunk := range []string{" ", "A", "V\n ", " Україна", "  "} {
		chunks.Write([]byte(chunk))
	}
	if *whole.Width() != *chunks.Width() || whole.approximate {
		t.Fatal("chunking or supported Cyrillic changed font metrics")
	}
	fallback := serpWidthCounter{pixels: 20}
	fallback.Write([]byte("\U0001f680"))
	if !fallback.approximate || *fallback.Width() != 20 {
		t.Fatal("unsupported glyph must use marked one-em fallback")
	}
}

func TestSERPMetricsBeforeStorageTruncation(t *testing.T) {
	title := strings.Repeat("W", 800)
	description := strings.Repeat("i", 5000)
	body := `<html><head><title>` + title + `</title><meta name="description" content="` + description + `"></head><body>Test</body></html>`
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
	data, err := ParsePage(response, "https://example.com", int64(len(body)+1), int64(len(body)+1))
	if err != nil {
		t.Fatal(err)
	}
	if *data.TitleCharCount != 800 || *data.DescriptionCharCount != 5000 || !data.TitleTruncated {
		t.Fatal("original character counts were lost")
	}
	if len(data.Title) != StorageTitleMaxRunes || len(data.Description) != StorageDescriptionMaxRunes {
		t.Fatal("storage bounds changed")
	}
	want := serpWidthCounter{pixels: 14}
	want.Write([]byte(description))
	if *data.DescriptionWidthPX != *want.Width() || data.TitleStatus != "High truncation risk" || data.DescriptionMobileStatus != "Likely truncate" || data.SERPWidthModel != SERPWidthModel {
		t.Fatalf("incorrect full metadata measurements: %+v", data)
	}
}

func TestFailedParseDoesNotPublishSERPMetrics(t *testing.T) {
	body := `<title>Partial</title><script>` + strings.Repeat("x", 1024) + `</script>`
	response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	data, err := ParsePage(response, "https://example.com", 2048, 256)
	if err == nil || data.TitleWidthPX != nil || data.DescriptionWidthPX != nil || data.SERPWidthModel != "" {
		t.Fatal("incomplete HTML must not produce successful SERP metrics")
	}
}
