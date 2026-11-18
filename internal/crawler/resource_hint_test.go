package crawler

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestResourceExtensionIsOnlyADiscoveryHint(t *testing.T) {
	for _, raw := range []string{"https://example.com/photo.JPG?token=private", "https://example.com/photo%2Epng", "https://example.com/file.pdf", "https://example.com/app.js"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !HasResourceExtension(u.Path) {
			t.Fatalf("resource hint missing for %s", raw)
		}
	}
	for _, raw := range []string{"https://example.com/page", "https://example.com/page.html?image=x.jpg", "https://example.com/products?start=40", "https://example.com/image.jpg/", "https://example.com/data.php"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if HasResourceExtension(u.Path) {
			t.Fatalf("document URL filtered: %s", raw)
		}
	}
}

func TestGenericURLShapeKeepsCrawlHintsOutOfOtherImports(t *testing.T) {
	raw, err := json.Marshal(DescribeURL("https://example.com/image.png?page=40"))
	if err != nil || strings.Contains(string(raw), "resource_hint") {
		t.Fatalf("generic URL shape changed: %s %v", raw, err)
	}
}
