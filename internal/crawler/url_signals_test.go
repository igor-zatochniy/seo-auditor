package crawler

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalHeaderParsingIsBoundedAndQuoteAware(t *testing.T) {
	s := CanonicalHeaders([]string{`<https://example.com/a?x=1,2>; rel="alternate canonical"; title="A, B", <https://example.com/feed>; rel=alternate`})
	if !s.Complete || len(s.HTTP) != 1 || s.HTTP[0].URL != "https://example.com/a?x=1,2" {
		t.Fatalf("signals=%+v", s)
	}
	for _, value := range []string{`<https://example.com/>; rel="canonical`, strings.Repeat("x", 8193), `<https://example.com/>; rel=canonical; anchor="https://other.example/"`} {
		if CanonicalHeaders([]string{value}).Complete {
			t.Fatal("incomplete header accepted")
		}
	}
	s = CanonicalHeaders([]string{strings.Repeat(`<https://example.com/>; rel=canonical,`, 12)})
	if s.Complete || len(s.HTTP) != 8 {
		t.Fatal("canonical budget ignored")
	}
}

func TestURLRoutesDistinguishSelfCanonicalCyclesAndMissingTargets(t *testing.T) {
	ok := 200
	redirect := 301
	ref := func(fp string) *URLReference {
		return &URLReference{URL: "https://example.com/" + fp, Fingerprints: []string{fp}, Valid: true}
	}
	nodes := map[string]RouteNode{"a": {ID: 1, HTTP: &ok, Status: "completed", KeyID: "key", Fingerprint: "a", Signals: &URLSignals{Complete: true, HTML: ref("a")}}, "b": {ID: 2, HTTP: &redirect, Status: "redirect", KeyID: "key", Fingerprint: "b", Signals: &URLSignals{Complete: true, Redirect: ref("c")}}, "c": {ID: 3, HTTP: &redirect, Status: "redirect", KeyID: "key", Fingerprint: "c", Signals: &URLSignals{Complete: true, Redirect: ref("b")}}}
	lookup := func(key, fp string) (RouteNode, bool) { n, ok := nodes[fp]; return n, ok && key == "key" }
	if r := TraceURLRoute(context.Background(), nodes["a"], "canonical", lookup); r.State != "resolved" || len(r.Steps) != 1 {
		t.Fatalf("self=%+v", r)
	}
	if r := TraceURLRoute(context.Background(), nodes["b"], "redirect", lookup); r.State != "loop" || len(r.Steps) != 3 {
		t.Fatalf("loop=%+v", r)
	}
	a := nodes["a"]
	a.Signals = &URLSignals{Complete: true, HTML: ref("missing")}
	if r := TraceURLRoute(context.Background(), a, "canonical", lookup); r.State != "not_audited" {
		t.Fatalf("missing=%+v", r)
	}
	a.Signals = &URLSignals{Complete: true}
	if r := TraceURLRoute(context.Background(), a, "canonical", lookup); r.State != "not_declared" {
		t.Fatal("missing canonical treated as self")
	}
	if r := TraceURLRoute(context.Background(), a, "redirect", lookup); r.State != "not_redirect" {
		t.Fatal("normal page treated as redirect")
	}
	a.Signals = &URLSignals{Complete: true, HTML: ref("b")}
	if r := TraceURLRoute(context.Background(), a, "canonical", lookup); r.State != "loop" {
		t.Fatal("canonical→redirect loop not found")
	}
}

func TestURLRouteBudgetsNoindexAndKeyIsolation(t *testing.T) {
	ok := 200
	nodes := map[string]RouteNode{}
	for i := 1; i <= 20; i++ {
		fp := fmt.Sprint(i)
		nodes[fp] = RouteNode{ID: int64(i), KeyID: "key", HTTP: &ok, Status: "completed", Signals: &URLSignals{Complete: true, HTML: &URLReference{URL: fp, Fingerprints: []string{fmt.Sprint(i + 1)}, Valid: true}}}
	}
	lookup := func(key, fp string) (RouteNode, bool) { n, exists := nodes[fp]; return n, exists && key == "key" }
	if r := TraceURLRoute(context.Background(), nodes["1"], "canonical", lookup); r.State != "hop_limit" || len(r.Steps) != MaxRouteHops+1 {
		t.Fatalf("budget=%+v", r)
	}
	n := nodes["2"]
	n.Noindex = true
	n.Signals = &URLSignals{Complete: true}
	nodes["2"] = n
	if r := TraceURLRoute(context.Background(), nodes["1"], "canonical", lookup); r.State != "noindex" {
		t.Fatal("noindex not found")
	}
	first := nodes["1"]
	first.KeyID = "other"
	if r := TraceURLRoute(context.Background(), first, "canonical", lookup); r.State != "not_audited" {
		t.Fatal("key isolation lost")
	}
}
