// Package render виконує JavaScript без прямого доступу браузера до мережі.
package render

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/performance"
)

const MaxRequests = 128
const MaxResourceBytes = 2 << 20
const MaxTransferBytes = 16 << 20

type Resource struct {
	Status int
	Header http.Header
	Body   []byte
}
type FetchResource func(context.Context, string) (Resource, error)
type Output struct {
	HTML     string
	Browser  string
	Requests int
	Blocked  int
	Bytes    int64
	Warnings []string
	Metrics  *performance.Result
	FinalURL string
}

// HTML передається з тієї самої HTTP-відповіді, яку вже перевірив основний crawler.
func Page(ctx context.Context, endpoint, target, userAgent string, response Resource, maxDOMBytes int64, get FetchResource) (out Output, err error) {
	return pageRun(ctx, endpoint, target, userAgent, response, maxDOMBytes, get, false)
}

func pageRun(ctx context.Context, endpoint, target, userAgent string, response Resource, maxDOMBytes int64, get FetchResource, measure bool) (out Output, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wsURL, err := debuggerURL(ctx, endpoint)
	if err != nil {
		return out, err
	}
	allocator, closeAllocator := chromedp.NewRemoteAllocator(ctx, wsURL)
	defer closeAllocator()
	root, closeRoot := chromedp.NewContext(allocator, chromedp.WithErrorf(func(string, ...any) {}))
	defer closeRoot()
	if err = chromedp.Run(root); err != nil {
		return out, fmt.Errorf("підключення до Chromium: %w", err)
	}
	tab, closeTab := chromedp.NewContext(root, chromedp.WithNewBrowserContext())
	defer closeTab()
	if err = chromedp.Run(tab); err != nil {
		return out, err
	}
	requests := make(chan *fetch.EventRequestPaused, 32)
	var overflow atomic.Bool
	var active atomic.Bool
	var scriptErrors atomic.Int64
	var networkErrors atomic.Int64
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	chromedp.ListenTarget(tab, func(event any) {
		if _, ok := event.(*network.EventLoadingFailed); ok {
			networkErrors.Add(1)
		}
		if _, ok := event.(*runtime.EventExceptionThrown); ok {
			scriptErrors.Add(1)
		}
		if request, ok := event.(*fetch.EventRequestPaused); ok {
			lastActivity.Store(time.Now().UnixNano())
			select {
			case requests <- request:
			default:
				overflow.Store(true)
				cancel()
			}
		}
	})
	finished := make(chan Output, 1)
	go func() {
		result := Output{}
		defer func() { finished <- result }()
		mainSent := false
		for {
			select {
			case <-tab.Done():
				return
			case request := <-requests:
				active.Store(true)
				result.Requests++
				var resource Resource
				var requestErr error
				isMain := !mainSent && request.ResourceType == network.ResourceTypeDocument && sameBrowserURL(request.Request.URL, target)
				if isMain {
					mainSent = true
					if measure {
						resource, requestErr = get(tab, request.Request.URL)
					} else {
						resource = response
					}
				} else if result.Requests > MaxRequests || request.Request.Method != http.MethodGet || !allowedResourceType(request.ResourceType) {
					requestErr = errors.New("ресурс заборонено політикою рендерингу")
				} else if result.Bytes >= MaxTransferBytes {
					requestErr = errors.New("ліміт завантаження ресурсів")
				} else {
					resource, requestErr = get(tab, request.Request.URL)
				}
				if requestErr != nil || result.Bytes+int64(len(resource.Body)) > MaxTransferBytes {
					result.Blocked++
					_ = chromedp.Run(tab, fetch.FailRequest(request.RequestID, network.ErrorReasonBlockedByClient))
				} else {
					result.Bytes += int64(len(resource.Body))
					headers := make([]*fetch.HeaderEntry, 0)
					for _, key := range []string{"Content-Type", "Content-Security-Policy", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "X-Content-Type-Options", "Location"} {
						for _, v := range resource.Header.Values(key) {
							headers = append(headers, &fetch.HeaderEntry{Name: key, Value: v})
						}
					}
					fulfill := fetch.FulfillRequest(request.RequestID, int64(resource.Status)).WithResponseHeaders(headers).WithBody(base64.StdEncoding.EncodeToString(resource.Body))
					if err := chromedp.Run(tab, fulfill); err != nil && tab.Err() == nil {
						result.Blocked++
					}
				}
				lastActivity.Store(time.Now().UnixNano())
				active.Store(false)
			}
		}
	}()
	defer func() {
		closeTab()
		result := <-finished
		out.Requests = result.Requests
		out.Blocked = result.Blocked
		out.Bytes = result.Bytes
		if n := networkErrors.Load(); n > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Ресурсів із помилкою завантаження: %d.", n))
		}
		if n := scriptErrors.Load(); n > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Необроблених помилок JavaScript: %d.", n))
		}
		if overflow.Load() {
			err = errors.New("перевищено чергу ресурсів Chromium")
		}
	}()
	err = chromedp.Run(tab,
		network.Enable(), network.SetCacheDisabled(true), network.SetBypassServiceWorker(true),
		emulation.SetUserAgentOverride(userAgent), emulation.SetDeviceMetricsOverride(1280, 900, 1, false),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if measure {
				return installPerformanceObserver(ctx)
			}
			return nil
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, product, _, _, _, e := browser.GetVersion().Do(ctx)
			out.Browser = product
			return e
		}),
		chromedp.Navigate(target),
	)
	if err != nil {
		return out, err
	}
	// Обмежене вікно після load: мінімум 2 с, quiet 500 ms, максимум 5 с.
	start := time.Now()
	for time.Since(start) < 5*time.Second {
		if !measure && time.Since(start) >= 2*time.Second && !active.Load() && len(requests) == 0 && time.Since(time.Unix(0, lastActivity.Load())) >= 500*time.Millisecond {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if time.Since(start) >= 5*time.Second && (active.Load() || len(requests) > 0 || time.Since(time.Unix(0, lastActivity.Load())) < 500*time.Millisecond) {
		out.Warnings = append(out.Warnings, "Сторінка продовжувала мережеву активність наприкінці вікна спостереження.")
	}
	if measure {
		err = chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error { return readPerformance(ctx, &out) }))
		if err == nil && !sameBrowserURL(out.FinalURL, target) {
			err = errors.New("адреса документа змінилася під час вимірювання")
		}
		return out, err
	}
	var snapshot struct {
		HTML      string `json:"html"`
		Oversized bool   `json:"oversized"`
		Shadow    bool   `json:"shadow"`
		URL       string `json:"url"`
	}
	// Ліміт перевіряється до передачі DOM через CDP; обхід вузлів також обмежений.
	expression := fmt.Sprintf(`(() => {
 let units=0,nodes=0,shadow=false;
 const walker=document.createTreeWalker(document,NodeFilter.SHOW_ELEMENT|NodeFilter.SHOW_TEXT|NodeFilter.SHOW_COMMENT);
 while(walker.nextNode()){
  const n=walker.currentNode;if(++nodes>100000)return {oversized:true};
  if(n.nodeType===1){units+=n.tagName.length*2+5;shadow ||= !!n.shadowRoot;for(const a of n.attributes)units+=a.name.length+a.value.length+4;}
  else units+=n.nodeValue.length;
  if(units>%d)return {oversized:true};
 }
 const html=document.documentElement.outerHTML;
 if(new TextEncoder().encode(html).length>%d)return {oversized:true};
 return {html,shadow,url:location.href};
})()`, maxDOMBytes, maxDOMBytes)
	err = chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		frame, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		// Сторінка не може підмінити DOM API або TextEncoder у цьому контексті.
		world, err := page.CreateIsolatedWorld(frame.Frame.ID).WithWorldName("seo-auditor-snapshot").Do(ctx)
		if err != nil {
			return err
		}
		return chromedp.Evaluate(expression, &snapshot, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithContextID(world)
		}).Do(ctx)
	}))
	if err != nil {
		return out, err
	}
	if snapshot.Oversized {
		return out, errors.New("DOM перевищив ліміт розміру або 100000 вузлів")
	}
	if !sameBrowserURL(snapshot.URL, target) {
		return out, errors.New("JavaScript змінив адресу документа; порівняння URL не виконано")
	}
	if snapshot.Shadow {
		out.Warnings = append(out.Warnings, "Shadow DOM не входить до серіалізованого light DOM.")
	}
	out.HTML = snapshot.HTML
	_ = chromedp.Run(tab, page.StopLoading())
	return out, nil
}

func allowedResourceType(kind network.ResourceType) bool {
	switch kind {
	case network.ResourceTypeScript, network.ResourceTypeStylesheet, network.ResourceTypeXHR, network.ResourceTypeFetch, network.ResourceTypeFont, network.ResourceTypeImage:
		return true
	}
	return false
}

func sameBrowserURL(left, right string) bool {
	normalize := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			return ""
		}
		h, e := crawler.NormalizeAuthority(u)
		if e != nil {
			return ""
		}
		u.Host = h
		u.Fragment = ""
		u.RawFragment = ""
		if u.Path == "" {
			u.Path = "/"
		}
		return u.String()
	}
	l, r := normalize(left), normalize(right)
	return l != "" && l == r
}

func debuggerURL(ctx context.Context, endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil {
		return "", errors.New("некоректна адреса Chromium")
	}
	u.Path = "/json/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	// CDP приймає лише IP/localhost у Host; TCP-адреса лишається налаштованим сервісом.
	req.Host = "localhost"
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("браузер Chromium недоступний: %w", err)
	}
	defer resp.Body.Close()
	var data struct {
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if resp.StatusCode != 200 {
		return "", errors.New("браузер Chromium повернув некоректний HTTP-статус")
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&data); err != nil {
		return "", errors.New("некоректна CDP-відповідь")
	}
	ws, err := url.Parse(data.WebSocket)
	if err != nil || ws.Scheme != "ws" || !strings.HasPrefix(ws.Path, "/devtools/browser/") {
		return "", errors.New("відсутній browser endpoint CDP")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	ws.Host = net.JoinHostPort(u.Hostname(), port)
	ws.User = nil
	ws.RawQuery = ""
	ws.Fragment = ""
	return ws.String(), nil
}
