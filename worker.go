package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

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
		resp.Body.Close()
		reqCancel()

		if data.GEO == nil {
			data.GEO = &geo.Signals{}
		}
		data.GEO.HTTP = baseData.GEO.HTTP
		data.GEO.Search.GooglebotRules = searchRules.GooglebotRules
		data.GEO.Search.OAISearchBotRules = searchRules.OAISearchBotRules
		data.GEO.Search.GPTBotRules = searchRules.GPTBotRules
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
