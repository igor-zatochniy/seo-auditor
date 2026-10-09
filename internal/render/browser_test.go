package render

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestResourceTypesAreReadOnly(t *testing.T) {
	for _, kind := range []network.ResourceType{network.ResourceTypeDocument, network.ResourceTypeWebSocket, network.ResourceTypePing, network.ResourceTypeMedia, network.ResourceTypeOther} {
		if allowedResourceType(kind) {
			t.Fatalf("allowed %s", kind)
		}
	}
}

func browserEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("RENDER_TEST_BROWSER_URL")
	if endpoint == "" {
		t.Skip("RENDER_TEST_BROWSER_URL не налаштовано")
	}
	return endpoint
}

func TestBrowserUsesCapturedHTMLAndBroker(t *testing.T) {
	endpoint := browserEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	calls := 0
	body := `<html><head><title>Before</title></head><body><script src="/app.js"></script></body></html>`
	out, err := Page(ctx, endpoint, "https://example.com/page", "SEO-Test", Resource{200, http.Header{"Content-Type": []string{"text/html"}}, []byte(body)}, 1<<20, func(ctx context.Context, u string) (Resource, error) {
		calls++
		if u != "https://example.com/app.js" {
			return Resource{}, errors.New("unexpected resource")
		}
		return Resource{200, http.Header{"Content-Type": []string{"text/javascript"}}, []byte(`document.title="After";document.body.insertAdjacentHTML("beforeend","<h1>Rendered heading</h1>")`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.HTML, "<title>After</title>") || !strings.Contains(out.HTML, "Rendered heading") || calls != 1 || out.Blocked != 0 {
		t.Fatalf("bad output: %+v calls=%d", out, calls)
	}
}

func TestBrowserTimeoutAndDOMLimit(t *testing.T) {
	endpoint := browserEndpoint(t)
	for _, tc := range []struct {
		name, body string
		limit      int64
		timeout    time.Duration
	}{
		{"infinite_script", `<script>while(true){}</script>`, 4096, 2 * time.Second},
		{"large_dom", `<script>document.write('x'.repeat(10000))</script>`, 4096, 30 * time.Second},
		{"tampered_dom_guards", `<script>document.write('x'.repeat(10000));document.createTreeWalker=()=>({nextNode:()=>false});window.TextEncoder=class{encode(){return []}};</script>`, 4096, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			start := time.Now()
			_, err := Page(ctx, endpoint, "https://example.com/page", "SEO-Test", Resource{200, http.Header{"Content-Type": []string{"text/html"}}, []byte(tc.body)}, tc.limit, func(context.Context, string) (Resource, error) { return Resource{}, errors.New("blocked") })
			if err == nil {
				t.Fatal("missing limit error")
			}
			if tc.name != "infinite_script" && !strings.Contains(err.Error(), "DOM перевищив") {
				t.Fatalf("wrong failure: %v", err)
			}
			if tc.name == "infinite_script" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wrong timeout: %v", err)
			}
			if time.Since(start) > tc.timeout+5*time.Second {
				t.Fatal("cleanup exceeded timeout margin")
			}
		})
	}
}

func TestBrowserURLNormalization(t *testing.T) {
	if !sameBrowserURL("https://bücher.de", "https://xn--bcher-kva.de/") || !sameBrowserURL("https://example.com/a#ready", "https://example.com/a") || sameBrowserURL("https://example.com/a", "https://example.com/b") {
		t.Fatal("browser URL mismatch")
	}
}

func TestMalformedDebuggerEndpointReturnsError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"healthy"}`)) }))
	defer s.Close()
	if _, err := debuggerURL(context.Background(), s.URL); err == nil {
		t.Fatal("accepted non-CDP endpoint")
	}
}

func TestDebuggerEndpointUsesLocalHostHeaderAndConfiguredAddress(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost" || r.URL.Path != "/json/version" {
			t.Errorf("unexpected CDP request: host=%q path=%q", r.Host, r.URL.Path)
		}
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://untrusted.example/devtools/browser/test"}`))
	}))
	defer s.Close()
	got, err := debuggerURL(context.Background(), s.URL)
	want := strings.Replace(s.URL, "http://", "ws://", 1) + "/devtools/browser/test"
	if err != nil || got != want {
		t.Fatalf("CDP must use configured transport address: got=%q err=%v", got, err)
	}
}

func TestBrowserBlocksWritesAndNavigation(t *testing.T) {
	endpoint := browserEndpoint(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := `<body><script>fetch('https://example.com/write',{method:'POST',body:'unsafe'}).catch(()=>{});const f=document.createElement('iframe');f.src='http://127.0.0.1:9222/json';document.body.append(f);</script></body>`
	out, err := Page(ctx, endpoint, "https://example.com/page", "SEO-Test", Resource{200, http.Header{"Content-Type": []string{"text/html"}}, []byte(body)}, 1<<20, func(context.Context, string) (Resource, error) {
		t.Error("write or frame reached network broker")
		return Resource{}, errors.New("blocked")
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Blocked < 1 {
		t.Fatalf("no blocked resources: %+v", out)
	}
}
