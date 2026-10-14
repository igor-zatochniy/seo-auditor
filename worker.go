package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/igor-zatochniy/seo-auditor/internal/seo"

	"github.com/jackc/pgx/v5/pgxpool"
)

func worker(
	schedulingCtx context.Context,
	operationCtx context.Context,
	id int,
	jobs <-chan AuditTarget,
	results chan<- Result,
	pageClient *http.Client,
	robotsClient *http.Client,
	robotsCache *robotsPolicyCache,
	dbPool *pgxpool.Pool,
	cfg Config,
	wg *sync.WaitGroup,
	onFatal ...func(error),
) {
	defer wg.Done()
	workerLogger := slog.With("worker_id", id)

	for {
		var target AuditTarget
		select {
		case <-schedulingCtx.Done():
			workerLogger.Debug("Worker не приймає нові задачі після сигналу зупинки")
			return
		case queuedTarget, ok := <-jobs:
			if !ok {
				return
			}
			target = queuedTarget
		}
		if schedulingCtx.Err() != nil {
			workerLogger.Debug("Worker залишає заплановану задачу для наступного запуску")
			return
		}

		if err := markAuditRunTargetStarted(operationCtx, dbPool, target, cfg); err != nil {
			workerLogger.Warn("Не вдалося позначити target як running", "target_id", target.TargetID, "url", target.SafeURL, "error", err)
			if len(onFatal) > 0 && onFatal[0] != nil {
				onFatal[0](err)
			}
			continue
		}
		start := time.Now()

		allowed, searchRules, err := robotsCache.inspectTarget(operationCtx, robotsClient, target.RequestURL, cfg.RobotsTotalTimeout)
		if err != nil {
			wrappedErr := fmt.Errorf("worker %d cannot verify robots.txt for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(SEOData{
				GEO:           &geo.Signals{Search: searchRules},
				URL:           target.RequestURL,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeUnavailable,
				Duration:      time.Since(start),
			}, errorCodeRobotsUnavailable, wrappedErr)
			result.Target = target
			results <- result
			continue
		}
		if !allowed {
			workerLogger.Warn("Сканування URL заборонено правилами robots.txt", "target_id", target.TargetID, "url", target.SafeURL)
			results <- Result{Target: target, Data: SEOData{
				GEO:           &geo.Signals{Search: searchRules},
				URL:           target.RequestURL,
				ScanStatus:    scanStatusBlockedByRobots,
				RobotsAllowed: false,
				RobotsOutcome: robotsOutcomeDisallowed,
				Duration:      time.Since(start),
			}}
			continue
		}
		baseData := SEOData{
			GEO:           &geo.Signals{Search: searchRules},
			URL:           target.RequestURL,
			RobotsAllowed: true,
			RobotsOutcome: robotsOutcomeAllowed,
		}
		var baseline *conditionalBaseline
		if cfg.ConditionalRequests {
			lookupCtx, cancel := context.WithTimeout(operationCtx, effectiveDBFetchTimeout(cfg))
			baseline, err = loadConditionalBaseline(lookupCtx, dbPool, cfg, target)
			cancel()
			if err != nil {
				workerLogger.Warn("Умовний HTTP-кеш недоступний; виконується повна перевірка", "target_id", target.TargetID, "error", sanitizeError(err))
				baseline = nil
			}
		}

		reqCtx, reqCancel := context.WithTimeout(operationCtx, cfg.HTTPTotalTimeout)
		reqCtx, httpObservation := observeHTTP(reqCtx)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.RequestURL, nil)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot create request for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeRequestCreationFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}
		req.Header.Set("User-Agent", UserAgentStr)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		if baseline != nil {
			if baseline.ETag != "" {
				req.Header.Set("If-None-Match", baseline.ETag)
			}
			if baseline.LastModified != "" {
				req.Header.Set("If-Modified-Since", baseline.LastModified)
			}
		}

		resp, err := pageClient.Do(req)
		if err != nil {
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d network request failed for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeRequestFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}
		baseData.XRobotsTag, baseData.XRobotsTagTruncated, baseData.XRobotsTagOriginalLength =
			robotsHeaderDirectives(resp.Header)
		baseData.GEO.HTTP = httpObservation(resp.StatusCode)
		baseData.HTTPResponseStatus = httpStatus(resp.StatusCode)
		if resp.StatusCode == http.StatusNotModified {
			changedHeader := len(resp.Header.Values("X-Robots-Tag")) > 0 && baseData.XRobotsTag != func() string {
				if baseline != nil {
					return baseline.Data.XRobotsTag
				}
				return ""
			}()
			changedHeader = changedHeader || canonicalHeaderChanged(resp.Header.Values("Link"), baseline, cfg)
			if baseline != nil && !changedHeader {
				data := baseline.Data
				data.URL = target.RequestURL
				data.Duration = time.Since(start)
				data.HTTPResponseStatus = httpStatus(304)
				data.ReusedFromRunID = baseline.RunID
				data.RobotsAllowed = true
				data.RobotsOutcome = robotsOutcomeAllowed
				data.HTTPETag = baseline.ETag
				data.HTTPLastModified = baseline.LastModified
				if value := safeHTTPValidator(resp.Header.Get("ETag"), 1024); value != "" {
					data.HTTPETag = value
				}
				if value := safeHTTPValidator(resp.Header.Get("Last-Modified"), 128); value != "" {
					data.HTTPLastModified = value
				}
				if data.GEO != nil {
					data.GEO.HTTP = baseData.GEO.HTTP
					data.GEO.Search.GooglebotRules = searchRules.GooglebotRules
					data.GEO.Search.OAISearchBotRules = searchRules.OAISearchBotRules
					data.GEO.Search.GPTBotRules = searchRules.GPTBotRules
					data.GEO.Search.AIPolicies = searchRules.AIPolicies
				}
				resp.Body.Close()
				reqCancel()
				results <- Result{Target: target, Data: data}
				continue
			}
			resp.Body.Close()
			req.Header.Del("If-None-Match")
			req.Header.Del("If-Modified-Since")
			resp, err = pageClient.Do(req)
			if err != nil {
				reqCancel()
				baseData.Duration = time.Since(start)
				result := failedScanResult(baseData, errorCodeRequestFailed, fmt.Errorf("unconditional revalidation failed: %s", sanitizeError(err)))
				result.Target = target
				results <- result
				continue
			}
			if resp.StatusCode == http.StatusNotModified {
				resp.Body.Close()
				reqCancel()
				baseData.StatusCode = httpStatus(304)
				baseData.Duration = time.Since(start)
				result := failedScanResult(baseData, errorCodeResponseParseFailed, fmt.Errorf("server returned 304 without valid conditional baseline"))
				result.Target = target
				results <- result
				continue
			}
			baseData.XRobotsTag, baseData.XRobotsTagTruncated, baseData.XRobotsTagOriginalLength = robotsHeaderDirectives(resp.Header)
			baseData.HTTPResponseStatus = httpStatus(resp.StatusCode)
			baseData.GEO.HTTP = httpObservation(resp.StatusCode)
		}

		baseData.URLSignals = crawler.CanonicalHeaders(resp.Header.Values("Link"))
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			redirectURL := resp.Header.Get("Location")
			safeRedirectURL := redactURL(redirectURL)
			resp.Body.Close()
			reqCancel()

			workerLogger.Info(
				"Виявлено HTTP-редирект",
				"from",
				target.SafeURL,
				"to",
				safeRedirectURL,
				"status",
				resp.StatusCode,
			)

			baseData.StatusCode = httpStatus(resp.StatusCode)
			baseData.ScanStatus = scanStatusRedirect
			baseData.IsRedirect = true
			baseData.RedirectURL = redirectURL
			baseData.Duration = time.Since(start)
			results <- Result{Target: target, Data: baseData}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			baseData.StatusCode = httpStatus(resp.StatusCode)
			baseData.ScanStatus = scanStatusCompleted
			baseData.Duration = time.Since(start)
			resp.Body.Close()
			reqCancel()
			workerLogger.Info("Збережено HTTP-статус без HTML-парсингу", "target_id", target.TargetID, "url", target.SafeURL, "status", resp.StatusCode)
			results <- Result{Target: target, Data: baseData}
			continue
		}

		baseData.StatusCode = httpStatus(resp.StatusCode)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			resp.Body.Close()
			reqCancel()
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d rejected %s: missing Content-Type header", id, target.SafeURL)
			result := failedScanResult(baseData, errorCodeMissingContentType, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		if err := validateHTMLContentType(contentType); err != nil {
			resp.Body.Close()
			reqCancel()
			workerLogger.Warn("Пропущено непідтримуваний тип контенту", "target_id", target.TargetID, "url", target.SafeURL, "content_type", contentType)
			baseData.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d skipped unsupported content type %q for %s: %s", id, contentType, target.SafeURL, sanitizeError(err))
			result := failedScanResult(baseData, errorCodeUnsupportedContentType, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		maxHTMLTokenBytes := cfg.MaxHTMLTokenBytes
		if maxHTMLTokenBytes <= 0 {
			maxHTMLTokenBytes = DefaultMaxHTMLTokenBytes
		}
		parse := parsePage
		if target.DiscoverLinks {
			parse = seo.ParsePageWithLinks
		}
		var capture renderCapture
		if cfg.RenderJavaScript {
			capture.limit = cfg.MaxHTMLBodyBytes
			resp.Body = &capturedResponseBody{Reader: io.TeeReader(resp.Body, &capture), Closer: resp.Body}
			parse = func(r *http.Response, u string, b, t int64) (SEOData, error) {
				return seo.ParsePageForComparison(r, u, b, t, target.DiscoverLinks)
			}
		}
		data, err := parse(
			resp,
			target.RequestURL,
			cfg.MaxHTMLBodyBytes,
			maxHTMLTokenBytes,
		)
		data.HTTPResponseStatus = httpStatus(resp.StatusCode)
		data.URLSignals = baseData.URLSignals
		data.HTTPETag = safeHTTPValidator(resp.Header.Get("ETag"), 1024)
		data.HTTPLastModified = safeHTTPValidator(resp.Header.Get("Last-Modified"), 128)
		resp.Body.Close()
		reqCancel()

		if data.GEO == nil {
			data.GEO = &geo.Signals{}
		}
		data.GEO.HTTP = baseData.GEO.HTTP
		data.GEO.Search.GooglebotRules = searchRules.GooglebotRules
		data.GEO.Search.OAISearchBotRules = searchRules.OAISearchBotRules
		data.GEO.Search.GPTBotRules = searchRules.GPTBotRules
		data.GEO.Search.AIPolicies = searchRules.AIPolicies
		if err != nil {
			data.RobotsAllowed = true
			data.RobotsOutcome = robotsOutcomeAllowed
			data.Duration = time.Since(start)
			wrappedErr := fmt.Errorf("worker %d cannot parse HTML for %s: %s", id, target.SafeURL, sanitizeError(err))
			result := failedScanResult(data, errorCodeResponseParseFailed, wrappedErr)
			result.Target = target
			results <- result
			continue
		}

		data.ScanStatus = scanStatusCompleted
		if cfg.RenderJavaScript {
			data.Rendering = renderPageComparison(operationCtx, cfg, target.RequestURL, capture.Bytes(), resp.Header, data, pageClient, robotsClient, robotsCache)
			data.Rendering.Performance = measurePagePerformance(operationCtx, cfg, target.RequestURL, pageClient, robotsClient, robotsCache)
		}
		if data.GEO != nil {
			if data.Rendering != nil {
				data.GEO.Performance = data.Rendering.Performance
			}
		}
		data.RobotsAllowed = true
		data.RobotsOutcome = robotsOutcomeAllowed
		data.Duration = time.Since(start)

		select {
		case <-operationCtx.Done():
			workerLogger.Warn("Відправлення результату скасовано після вичерпання shutdown timeout", "target_id", target.TargetID, "url", target.SafeURL)
			return
		case results <- Result{Target: target, Data: data}:
		}
	}
}

type capturedResponseBody struct {
	io.Reader
	io.Closer
}
