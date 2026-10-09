package render

import (
	"context"
	_ "embed"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/igor-zatochniy/seo-auditor/internal/performance"
)

//go:embed vendor/web-vitals.iife.js
var webVitals string

const performanceWorld = "seo-auditor-performance"

func installPerformanceObserver(ctx context.Context) error {
	script := webVitals + `
globalThis.__seoMetrics={lcp_ms:null,cls:null};
webVitals.onLCP(m=>{globalThis.__seoMetrics.lcp_ms=m.value;},{reportAllChanges:true});
webVitals.onCLS(m=>{globalThis.__seoMetrics.cls=m.value;},{reportAllChanges:true});
`
	_, err := page.AddScriptToEvaluateOnNewDocument(script).WithWorldName(performanceWorld).Do(ctx)
	return err
}

func readPerformance(ctx context.Context, out *Output) error {
	frame, err := page.GetFrameTree().Do(ctx)
	if err != nil {
		return err
	}
	world, err := page.CreateIsolatedWorld(frame.Frame.ID).WithWorldName(performanceWorld).Do(ctx)
	if err != nil {
		return err
	}
	var measured struct {
		LCP     *float64 `json:"lcp_ms"`
		CLS     *float64 `json:"cls"`
		Elapsed float64  `json:"elapsed"`
		URL     string   `json:"url"`
	}
	err = chromedp.Evaluate(`({...globalThis.__seoMetrics,elapsed:performance.now(),url:location.href})`, &measured,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithContextID(world) }).Do(ctx)
	if err != nil {
		return err
	}
	out.Metrics = &performance.Result{Status: "completed", LCPMS: measured.LCP, CLS: measured.CLS, ObservedMS: int64(measured.Elapsed)}
	out.FinalURL = measured.URL
	return nil
}

// Measure робить окреме завантаження через той самий захищений Go-брокер.
func Measure(ctx context.Context, endpoint, target, userAgent string, get FetchResource) (*performance.Result, error) {
	start := time.Now()
	out, err := pageRun(ctx, endpoint, target, userAgent, Resource{}, 0, get, true)
	r := out.Metrics
	if r == nil {
		r = &performance.Result{Status: "failed", ObservedMS: time.Since(start).Milliseconds()}
	}
	r.Requests, r.Blocked, r.Warnings = out.Requests, out.Blocked, out.Warnings
	if err != nil {
		r.Status = "failed"
	}
	r.Classify()
	return r, err
}
