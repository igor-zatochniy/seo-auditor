package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/performance"
	"github.com/igor-zatochniy/seo-auditor/internal/render"
	"github.com/igor-zatochniy/seo-auditor/internal/seo"
)

func effectiveRenderTimeout(cfg Config) time.Duration {
	if cfg.RenderTimeout > 0 {
		return cfg.RenderTimeout
	}
	return 30 * time.Second
}

func validateRendering(cfg Config) error {
	if !cfg.RenderJavaScript {
		return nil
	}
	u, err := url.Parse(cfg.RenderBrowserURL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("для JavaScript потрібен RENDER_BROWSER_URL у форматі http://browser:9222")
	}
	if cfg.TargetLeaseDuration <= cfg.RobotsTotalTimeout+cfg.HTTPTotalTimeout+2*effectiveRenderTimeout(cfg)+5*time.Second {
		return errors.New("TARGET_LEASE_DURATION має перевищувати robots + HTTP + два browser budgets щонайменше на 5s")
	}
	return nil
}

type renderCapture struct {
	bytes.Buffer
	limit int64
}

func (b *renderCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - int64(b.Len())
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(int64(len(p)), remaining)])
	}
	return n, nil
}

func renderPageComparison(ctx context.Context, cfg Config, target string, body []byte, headers http.Header, raw SEOData, pageClient, robotsClient *http.Client, cache *robotsPolicyCache) *seo.RenderComparison {
	start := time.Now()
	result := &seo.RenderComparison{Status: "failed", Raw: seo.Snapshot(raw)}
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, effectiveRenderTimeout(cfg))
	defer cancel()
	out, err := render.Page(ctx, cfg.RenderBrowserURL, target, UserAgentStr, render.Resource{Status: 200, Header: headers, Body: body}, cfg.MaxHTMLBodyBytes,
		func(ctx context.Context, address string) (render.Resource, error) {
			return fetchRenderResource(ctx, address, cfg, pageClient, robotsClient, cache)
		})
	result.Requests = out.Requests
	result.Blocked = out.Blocked
	result.Bytes = out.Bytes
	result.Browser = out.Browser
	result.Warnings = out.Warnings
	if err != nil {
		result.Error = sanitizeError(err)
		return result
	}
	// Серіалізація DOM завжди UTF-8, HTTP-заголовки robots залишаються початковими.
	h := headers.Clone()
	h.Set("Content-Type", "text/html; charset=utf-8")
	data, err := seo.ParsePageForComparison(&http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(out.HTML))}, target, cfg.MaxHTMLBodyBytes, cfg.MaxHTMLTokenBytes, false)
	if err != nil {
		result.Error = sanitizeError(err)
		return result
	}
	result.Rendered = seo.Snapshot(data)
	result.Delta = seo.Compare(result.Raw, result.Rendered)
	result.Status = "completed"
	if out.Blocked > 0 || len(out.Warnings) > 0 || !result.Raw.Complete || !result.Rendered.Complete {
		result.Status = "partial"
	}
	return result
}

func fetchRenderResource(ctx context.Context, address string, cfg Config, pageClient, robotsClient *http.Client, cache *robotsPolicyCache) (render.Resource, error) {
	return fetchBrowserResource(ctx, address, cfg, pageClient, robotsClient, cache, render.MaxResourceBytes, false)
}

func measurePagePerformance(ctx context.Context, cfg Config, target string, pageClient, robotsClient *http.Client, cache *robotsPolicyCache) *performance.Result {
	ctx, cancel := context.WithTimeout(ctx, effectiveRenderTimeout(cfg))
	defer cancel()
	first := true
	r, err := render.Measure(ctx, cfg.RenderBrowserURL, target, UserAgentStr, func(ctx context.Context, address string) (render.Resource, error) {
		main := first
		first = false
		limit := int64(render.MaxResourceBytes)
		if main {
			limit = cfg.MaxHTMLBodyBytes
		}
		return fetchBrowserResource(ctx, address, cfg, pageClient, robotsClient, cache, limit, main)
	})
	if err != nil {
		r.Error = sanitizeError(err)
	}
	return r
}

func fetchBrowserResource(ctx context.Context, address string, cfg Config, pageClient, robotsClient *http.Client, cache *robotsPolicyCache, limit int64, mainDocument bool) (render.Resource, error) {
	normalized, err := normalizeTargetURL(address, cfg.AllowPrivateTargets)
	if err != nil {
		return render.Resource{}, err
	}
	allowed, err := cache.isAllowedByRobots(ctx, robotsClient, normalized, cfg.RobotsTotalTimeout)
	if err != nil || !allowed {
		return render.Resource{}, errors.New("robots.txt не дозволяє ресурс")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return render.Resource{}, err
	}
	req.Header.Set("User-Agent", UserAgentStr)
	resp, err := pageClient.Do(req)
	if err != nil {
		return render.Resource{}, err
	}
	if mainDocument && (resp.StatusCode != http.StatusOK || validateHTMLContentType(resp.Header.Get("Content-Type")) != nil) {
		resp.Body.Close()
		return render.Resource{}, errors.New("лабораторний прохід не отримав HTML з HTTP 200")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		location, err := resp.Location()
		if err != nil {
			return render.Resource{}, err
		}
		if _, err := normalizeTargetURL(location.String(), cfg.AllowPrivateTargets); err != nil {
			return render.Resource{}, err
		}
		header := resp.Header.Clone()
		header.Set("Location", location.String())
		return render.Resource{Status: resp.StatusCode, Header: header}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return render.Resource{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := seo.ReadLimited(resp.Body, limit)
	resp.Body.Close()
	if err != nil {
		return render.Resource{}, err
	}
	return render.Resource{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}
