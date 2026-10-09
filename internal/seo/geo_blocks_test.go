package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGEOAnswerBlocksAreBoundedAndContextual(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
		complete   bool
		heading    string
	}{
		{"too_short", "<p>" + strings.Repeat("я", 299) + "</p>", 0, true, ""},
		{"lower_unicode", "<h2>Як працює захист?</h2><p>" + strings.Repeat("я", 300) + "</p>", 1, true, "Як працює захист?"},
		{"upper", "<p>" + strings.Repeat("a", 500) + "</p>", 1, true, ""},
		{"too_long", "<p>" + strings.Repeat("a", 501) + "</p>", 0, true, ""},
		{"inline", "<p>" + strings.Repeat("я", 150) + "<em>" + strings.Repeat("ю", 150) + "</em></p>", 1, true, ""},
		{"whitespace", "<p>" + strings.Repeat("a", 150) + " \n\t " + strings.Repeat("b", 149) + "</p>", 1, true, ""},
		{"hidden", "<p hidden>" + strings.Repeat("a", 300) + "</p>", 0, true, ""},
		{"template", "<template><p>" + strings.Repeat("a", 300) + "</p></template>", 0, true, ""},
		{"main_preferred", "<nav><p>" + strings.Repeat("a", 300) + "</p></nav><main><article><header><h2>Авторська відповідь</h2></header><p>" + strings.Repeat("б", 300) + "</p></article></main>", 1, true, "Авторська відповідь"},
		{"bounded_sample", strings.Repeat("<p>"+strings.Repeat("a", 300)+"</p>", 20), 20, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := "<html><body>" + tc.body + "</body></html>"
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(html))}
			data, err := ParsePage(response, "https://example.com", 1<<20, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			s := data.GEO.Blocks
			if s == nil || s.Count != tc.want || s.Complete != tc.complete || len(s.Items) > 8 {
				t.Fatalf("bad sample: %+v", s)
			}
			if len(s.Items) > 0 && s.Items[0].Heading != tc.heading {
				t.Fatalf("heading: %q", s.Items[0].Heading)
			}
		})
	}
}
