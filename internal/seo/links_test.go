package seo

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLinkDiscoveryUsesBaseAndIgnoresInertContent(t *testing.T) {
	body := `<head><base href="/dir/"></head><body><a href="next#part" rel="nofollow">Read <b>more</b></a><template><a href="hidden">Hidden</a></template><a href="mailto:x@example.com">Mail</a><a href="https://user:pass@example.com">Private</a>`
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
	d, err := ParsePageWithLinks(resp, "https://example.com/", 4096, 4096)
	if err != nil || len(d.DiscoveredLinks) != 1 {
		t.Fatalf("links=%+v err=%v", d.DiscoveredLinks, err)
	}
	link := d.DiscoveredLinks[0]
	if link.URL != "https://example.com/dir/next" || link.Anchor != "Read more" || !link.Nofollow {
		t.Fatalf("link=%+v", link)
	}
}

func TestLinkDiscoveryIsBoundedAndOptIn(t *testing.T) {
	body := "<body>" + strings.Repeat(`<a href="/next">Next</a>`, MaxDiscoveredLinks+20)
	for _, collect := range []bool{false, true} {
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body))}
		d, err := parsePage(resp, "https://example.com/", 1<<20, 1<<20, collect)
		if err != nil {
			t.Fatal(err)
		}
		if collect && (len(d.DiscoveredLinks) != MaxDiscoveredLinks || !d.LinksTruncated) {
			t.Fatal("discovery is not bounded")
		}
		if !collect && len(d.DiscoveredLinks) != 0 {
			t.Fatal("list mode collected URLs")
		}
		if d.InternalLinksCount != MaxDiscoveredLinks+20 {
			t.Fatal("link metric changed")
		}
	}
}
