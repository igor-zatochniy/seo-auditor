package seo

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func duplicatePage(t *testing.T, source string) Data {
	t.Helper()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(source))}
	data, err := ParsePage(resp, "https://example.com/page", 8<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestContentSignatureUsesFullMainContent(t *testing.T) {
	text := strings.Repeat("A clear description of payment risk and merchant protection. ", 100)
	a := duplicatePage(t, `<html lang="en-US"><head><title>Title</title></head><body><nav>Navigation</nav><main><h1>Topic</h1><p>`+text+`alpha</p></main><footer>Footer</footer></body></html>`)
	b := duplicatePage(t, `<html lang="en-US"><head><title>Title</title></head><body><nav>Different menu</nav><main><h1>Topic</h1><p>`+text+`beta</p></main><footer>Different footer</footer></body></html>`)
	if a.GEO.Excerpt != b.GEO.Excerpt || !a.GEO.SampleTruncated {
		t.Fatal("fixture must differ beyond the GEO excerpt")
	}
	if a.ContentSignature.SHA256 == b.ContentSignature.SHA256 {
		t.Fatal("full text was truncated for duplicate detection")
	}
	if !a.ContentSignature.Complete || a.ContentSignature.Source != "main" || a.ContentSignature.Language != "en-US" || a.ContentSignature.Words < 800 {
		t.Fatal(a.ContentSignature)
	}
	if a.Title != "Title" || a.H1 != "Topic" || a.H1Count != 1 {
		t.Fatal("ordinary SEO metrics changed", a)
	}
}

func TestContentSignatureIgnoresBoilerplateAndTemplates(t *testing.T) {
	content := strings.Repeat("Merchant payment safety and evidence. ", 30)
	a := duplicatePage(t, `<body><main>`+content+`</main></body>`)
	b := duplicatePage(t, `<body><header>Header</header><nav>Menu</nav><main>`+content+`<script>bad text</script><style>css</style><template><h1>Hidden words</h1></template><aside>Sidebar</aside><p hidden>Hidden</p></main><footer>Footer</footer></body>`)
	if a.ContentSignature.SHA256 != b.ContentSignature.SHA256 {
		t.Fatal("non-main content contaminated signatures")
	}
	if a.ContentSignature.Words != b.ContentSignature.Words {
		t.Fatal(a.ContentSignature, b.ContentSignature)
	}
}

func TestContentSignatureInlineAndBlockBoundaries(t *testing.T) {
	a := duplicatePage(t, `<article><header><h1>Author</h1></header><p>Stra<b>ße</b></p><p>world</p></article>`)
	b := duplicatePage(t, `<article><header><h1>Author</h1></header><p>STRASSE world</p></article>`)
	if a.ContentSignature.SHA256 != b.ContentSignature.SHA256 || a.ContentSignature.Source != "article" {
		t.Fatal(a.ContentSignature, b.ContentSignature)
	}
	c := duplicatePage(t, `<body>word<br>another</body>`)
	if c.ContentSignature.Words != 2 {
		t.Fatal(c.ContentSignature)
	}
}

func TestContentSignatureIncompleteDataIsNotEligible(t *testing.T) {
	a := duplicatePage(t, `<main>`+strings.Repeat("x", 129)+` word</main>`)
	if a.ContentSignature.Complete {
		t.Fatal("overlong word considered complete")
	}
	a = duplicatePage(t, strings.Repeat("<div>", 130)+"word"+strings.Repeat("</div>", 130))
	if a.ContentSignature.Complete {
		t.Fatal("incomplete main-content structure considered complete")
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("<p>word</p>", 100)))}
	d, err := ParsePage(resp, "https://example.com/", 128, 128)
	if err == nil || d.ContentSignature != nil {
		t.Fatal("truncated HTML received a complete signature", d.ContentSignature, err)
	}
}

func BenchmarkContentSignatureParser(b *testing.B) {
	source := `<main>` + strings.Repeat("<p>Some useful content about payments, evidence and fraud.</p>", 10000) + `</main>`
	b.ReportAllocs()
	for b.Loop() {
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(source))}
		if _, err := ParsePage(resp, "https://example.com/", 8<<20, 1<<20); err != nil {
			b.Fatal(fmt.Errorf("parse: %w", err))
		}
	}
}
