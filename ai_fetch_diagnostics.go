package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"
)

const (
	aiFetchBodyLimit     = 512 << 10
	aiFetchRedirectLimit = 5
	aiFetchRecentImports = 20
)

func aiFetchDiagnosticTimeout(cfg Config) time.Duration {
	return 2*(cfg.HTTPTotalTimeout+cfg.RobotsTotalTimeout) + 5*time.Second
}

type simulatedBot struct {
	Token, Label, UserAgent string
}

var simulatedAIBots = map[string]simulatedBot{
	"googlebot":        {"Googlebot", "Googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"},
	"oai-searchbot":    {"OAI-SearchBot", "OAI-SearchBot", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36; compatible; OAI-SearchBot/1.4; +https://openai.com/searchbot"},
	"gptbot":           {"GPTBot", "GPTBot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.4; +https://openai.com/gptbot"},
	"claude-searchbot": {"Claude-SearchBot", "Claude-SearchBot", "Claude-SearchBot/1.0"},
	"claudebot":        {"ClaudeBot", "ClaudeBot", "ClaudeBot/1.0"},
	"perplexitybot":    {"PerplexityBot", "PerplexityBot", "PerplexityBot/1.0"},
	"applebot":         {"Applebot", "Applebot", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15 (Applebot/0.1; +http://www.apple.com/go/applebot)"},
}

type aiFetchObservation struct {
	Status             string   `json:"status"`
	Explanation        string   `json:"explanation"`
	HTTPStatus         int      `json:"http_status,omitempty"`
	ContentType        string   `json:"content_type,omitempty"`
	HTMLContentType    bool     `json:"html_content_type"`
	VisibleWords       int      `json:"visible_words,omitempty"`
	BodyBytes          int      `json:"body_bytes,omitempty"`
	BodyTruncated      bool     `json:"body_truncated,omitempty"`
	ChallengeSuspected bool     `json:"challenge_suspected"`
	ChallengeSignals   []string `json:"challenge_signals,omitempty"`
	RobotsStatus       string   `json:"robots_status"`
	Redirects          []string `json:"redirects"`
	FinalURL           string   `json:"final_url,omitempty"`
	DurationMS         int64    `json:"duration_ms"`
}

type aiFetchLogEvidence struct {
	Status         string     `json:"status"`
	ImportDomain   string     `json:"import_domain,omitempty"`
	Bot            string     `json:"bot,omitempty"`
	Verification   string     `json:"verification,omitempty"`
	Visits         int        `json:"visits,omitempty"`
	Failures       int        `json:"failures,omitempty"`
	Repeated429503 bool       `json:"repeated_429_503"`
	HTTPStatuses   []int      `json:"http_statuses,omitempty"`
	First          *time.Time `json:"first,omitempty"`
	Last           *time.Time `json:"last,omitempty"`
	Explanation    string     `json:"explanation"`
}

type aiFetchDiagnosticsReport struct {
	URL                 string               `json:"url"`
	Bot                 string               `json:"bot"`
	AuditorSimulation   aiFetchObservation   `json:"auditor_simulation"`
	BotSimulation       aiFetchObservation   `json:"bot_simulation"`
	RealLogObservations []aiFetchLogEvidence `json:"real_log_observations"`
	Discrepancy         bool                 `json:"discrepancy"`
	Disclaimer          string               `json:"disclaimer"`
}

func (s *webServer) aiFetchDiagnostics(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL string `json:"url"`
		Bot string `json:"bot"`
	}
	if !decodeWebJSON(w, r, 4096, &input) {
		return
	}
	bot, ok := simulatedAIBots[input.Bot]
	if !ok {
		writeWebError(w, 400, "invalid_bot", "Оберіть підтримуваного HTTP-бота")
		return
	}
	target, _, err := normalizeSiteURL(input.URL, false)
	if err != nil {
		writeWebError(w, 400, "invalid_url", "Потрібен публічний HTTP(S) URL без облікових даних")
		return
	}
	select {
	case s.submissions <- struct{}{}:
		defer func() { <-s.submissions }()
	default:
		writeWebError(w, 429, "busy", "Інша мережева перевірка вже виконується")
		return
	}
	cfg := s.cfg
	cfg.AllowPrivateTargets = false
	cfg.Workers = 1
	transport := newHTTPTransport(cfg, cfg.HTTPAttemptTimeout)
	defer transport.CloseIdleConnections()
	pageClient := &http.Client{Transport: transport, Timeout: cfg.HTTPTotalTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	robotsClient := newRobotsHTTPClient(transport)
	cache := newRobotsPolicyCache(cfg.RobotsCacheTTL, 4)

	budget := 2*(cfg.HTTPTotalTimeout+cfg.RobotsTotalTimeout) + 2*time.Second
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	probeTimeout := cfg.HTTPTotalTimeout + cfg.RobotsTotalTimeout
	auditorCtx, cancelAuditor := context.WithTimeout(ctx, probeTimeout)
	auditor := probeAIFetch(auditorCtx, target, "", UserAgentStr, pageClient, robotsClient, cache, cfg)
	cancelAuditor()
	botCtx, cancelBot := context.WithTimeout(ctx, probeTimeout)
	botResult := probeAIFetch(botCtx, target, bot.Token, bot.UserAgent, pageClient, robotsClient, cache, cfg)
	cancelBot()

	logs, err := s.matchAIFetchLogs(r.Context(), target)
	if err != nil {
		logs = []aiFetchLogEvidence{{Status: "unknown", Explanation: "Не вдалося прочитати імпортовані журнали; мережеву симуляцію показано окремо."}}
	}
	response := &aiFetchDiagnosticsReport{
		URL: redactURL(target), Bot: bot.Label, AuditorSimulation: auditor, BotSimulation: botResult,
		RealLogObservations: logs,
		Disclaimer:          "Запит виконується з мережі SEO Auditor із тестовим User-Agent. Це не відтворює IP, DNS, TLS, регіон, частоту чи WAF-профіль провайдера і не гарантує точність рядка User-Agent. Реальні відповіді ботів показуються лише з імпортованих серверних журналів.",
	}
	response.Discrepancy = fetchDiscrepancy(auditor, botResult)
	writeWebJSON(w, 200, response)
}

func probeAIFetch(ctx context.Context, target, policyToken, userAgent string, pageClient, robotsClient *http.Client, cache *robotsPolicyCache, cfg Config) (result aiFetchObservation) {
	started := time.Now()
	result = aiFetchObservation{Status: "unknown", RobotsStatus: "unknown", Redirects: []string{}}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	current := target
	for redirectCount := 0; redirectCount <= aiFetchRedirectLimit; redirectCount++ {
		allowed, controls, err := cache.inspectTarget(ctx, robotsClient, current, cfg.RobotsTotalTimeout)
		if err != nil {
			result.Status = "unknown"
			result.RobotsStatus = "unknown"
			result.Explanation = "Не вдалося надійно перевірити robots.txt; HTTP-запит не виконано."
			return result
		}
		if policyToken == "" {
			if !allowed {
				result.Status = "blocked_by_robots"
				result.RobotsStatus = geo.Blocked
				result.Explanation = "URL заборонено правилами для SEO Auditor; запит пропущено."
				return result
			}
			result.RobotsStatus = geo.Allowed
		} else {
			status := aiPolicyStatus(controls.AIPolicies, policyToken)
			result.RobotsStatus = status
			if status != geo.Allowed {
				result.Status = "blocked_by_robots"
				if status == geo.Unknown {
					result.Status = "unknown"
					result.Explanation = "Дозвіл вибраного бота в robots.txt невідомий; симуляційний запит пропущено."
				} else {
					result.Explanation = "URL заборонено правилами robots.txt для вибраного токена; симуляційний запит пропущено."
				}
				return result
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			result.Status, result.Explanation = "network_error", "Не вдалося сформувати HTTP-запит."
			return result
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		resp, err := pageClient.Do(req)
		if err != nil {
			result.Status = "network_error"
			result.Explanation = "HTTP-запит не завершився; перевірте DNS, TLS, мережеві обмеження або timeout."
			return result
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location, locationErr := resp.Location()
			resp.Body.Close()
			result.HTTPStatus = resp.StatusCode
			if locationErr != nil {
				result.Status, result.Explanation = "redirect_invalid", "Сервер повернув редирект без коректної адреси призначення."
				return result
			}
			next, _, normalizeErr := normalizeSiteURL(location.String(), false)
			if normalizeErr != nil {
				result.Status, result.Explanation = "redirect_invalid", "Адреса редиректу не пройшла перевірку безпеки."
				return result
			}
			if !sameFetchOrigin(target, next) {
				result.Status, result.FinalURL = "external_redirect", redactURL(next)
				result.Redirects = append(result.Redirects, "HTTP "+strconv.Itoa(resp.StatusCode)+" → "+redactURL(next))
				result.Explanation = "Редирект веде на інший origin і не виконувався."
				return result
			}
			result.Redirects = append(result.Redirects, "HTTP "+strconv.Itoa(resp.StatusCode)+" → "+redactURL(next))
			if redirectCount == aiFetchRedirectLimit {
				result.Status, result.Explanation = "redirect_limit", "Перевищено ліміт редиректів."
				return result
			}
			current = next
			continue
		}
		result.HTTPStatus = resp.StatusCode
		result.ContentType = safeContentType(resp.Header.Get("Content-Type"))
		result.HTMLContentType = validateHTMLContentType(resp.Header.Get("Content-Type")) == nil
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, aiFetchBodyLimit+1))
		resp.Body.Close()
		if readErr != nil {
			result.Status, result.Explanation = "body_read_error", "Не вдалося прочитати обмежену відповідь."
			return result
		}
		if len(body) > aiFetchBodyLimit {
			result.BodyTruncated = true
			body = body[:aiFetchBodyLimit]
		}
		result.BodyBytes = len(body)
		if result.HTMLContentType {
			result.VisibleWords = visibleHTMLWords(body)
		}
		result.ChallengeSuspected, result.ChallengeSignals = detectFetchChallenge(body, resp.StatusCode)
		result.FinalURL = redactURL(current)
		result.Status = classifyFetchResponse(resp.StatusCode, result.HTMLContentType, len(body), result.ChallengeSuspected)
		result.Explanation = fetchStatusExplanation(result.Status)
		return result
	}
	result.Status, result.Explanation = "redirect_limit", "Перевищено ліміт редиректів."
	return result
}

func aiPolicyStatus(controls []geo.AIControl, token string) string {
	for _, control := range controls {
		if strings.EqualFold(control.Token, token) {
			return control.Status
		}
	}
	return geo.Unknown
}

func sameFetchOrigin(a, b string) bool {
	left, errLeft := url.Parse(a)
	right, errRight := url.Parse(b)
	if errLeft != nil || errRight != nil || left.User != nil || right.User != nil {
		return false
	}
	leftHost, errLeft := normalizeFetchAuthority(left)
	rightHost, errRight := normalizeFetchAuthority(right)
	return errLeft == nil && errRight == nil && strings.EqualFold(left.Scheme, right.Scheme) && leftHost == rightHost
}

func normalizeFetchAuthority(u *url.URL) (string, error) {
	return crawler.NormalizeAuthority(u)
}

func classifyFetchResponse(status int, htmlType bool, bodyBytes int, challenge bool) string {
	if challenge {
		return "challenge_suspected"
	}
	if status == http.StatusTooManyRequests || status >= 500 || status == http.StatusForbidden {
		return "http_rejected"
	}
	if status < 200 || status >= 300 {
		return "http_error"
	}
	if !htmlType {
		return "non_html"
	}
	if bodyBytes == 0 {
		return "empty_html"
	}
	return "html_received"
}

func fetchStatusExplanation(status string) string {
	switch status {
	case "html_received":
		return "Отримано HTML-відповідь із мережі SEO Auditor; це не перевірка з мережі бота."
	case "challenge_suspected":
		return "У відповіді знайдено ознаки challenge або CAPTCHA; це евристика, перевірте відповідь вручну."
	case "http_rejected":
		return "Сервер відхилив запит або повернув тимчасову помилку HTTP."
	case "non_html":
		return "Успішна відповідь має не-HTML Content-Type."
	case "empty_html":
		return "Content-Type указує на HTML, але тіло відповіді порожнє."
	case "http_error":
		return "Сервер повернув неуспішний HTTP-статус."
	default:
		return "Результат HTTP-перевірки не підтверджено."
	}
}

func detectFetchChallenge(body []byte, status int) (bool, []string) {
	text := strings.ToLower(string(body))
	signals := []string{}
	for _, marker := range []struct{ value, code string }{
		{"cf-chl-", "cloudflare_challenge"},
		{"challenge-platform", "challenge_platform"},
		{"checking your browser", "browser_check"},
		{"verify you are human", "human_verification"},
		{"captcha required", "captcha_required"},
		{"complete the captcha", "captcha_required"},
	} {
		if strings.Contains(text, marker.value) {
			signals = append(signals, marker.code)
		}
	}
	if status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 {
		for _, marker := range []struct{ value, code string }{
			{"attention required", "attention_required"},
			{"access denied", "access_denied"},
		} {
			if strings.Contains(text, marker.value) {
				signals = append(signals, marker.code)
			}
		}
	}
	if strings.Contains(text, "captcha") && (status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500) {
		signals = append(signals, "captcha_marker")
	}
	return len(signals) > 0, signals
}

func visibleHTMLWords(body []byte) int {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	tokenizer.SetMaxBuf(aiFetchBodyLimit)
	ignored := []string{}
	words := 0
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			return words
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if tokenType == html.StartTagToken && (token.Data == "script" || token.Data == "style" || token.Data == "template") {
				ignored = append(ignored, token.Data)
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			for i := len(ignored) - 1; i >= 0; i-- {
				if ignored[i] == token.Data {
					ignored = ignored[:i]
					break
				}
			}
		case html.TextToken:
			if len(ignored) == 0 {
				words += len(strings.Fields(string(tokenizer.Text())))
			}
		}
	}
}

func safeContentType(value string) string {
	value = strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
	if len(value) > 120 {
		return value[:120]
	}
	return value
}

func fetchDiscrepancy(auditor, simulated aiFetchObservation) bool {
	if auditor.Status != "html_received" {
		return false
	}
	switch simulated.Status {
	case "challenge_suspected", "http_rejected", "http_error", "non_html", "empty_html", "network_error", "external_redirect", "redirect_limit", "redirect_invalid":
		return true
	default:
		return false
	}
}

func (s *webServer) matchAIFetchLogs(ctx context.Context, target string) ([]aiFetchLogEvidence, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	domain, err := geo.NormalizeDomain(u.Hostname())
	if err != nil {
		return []aiFetchLogEvidence{{Status: "not_available", Explanation: "Імпортовані журнали зіставляються лише для доменних імен; HTTP-симуляція завершилася окремо."}}, nil
	}
	keyID := s.cfg.TargetFingerprintKeyID
	if keyID == "" {
		keyID = DefaultTargetFingerprintKeyID
	}
	var importDomain string
	err = s.pool.QueryRow(ctx, `SELECT domain FROM audit_bot_imports WHERE ($1=domain OR $1 LIKE '%.'||domain) AND fingerprint_key_id=$2 ORDER BY created_at DESC,length(domain) DESC,id DESC LIMIT 1`, domain, keyID).Scan(&importDomain)
	if errors.Is(err, pgx.ErrNoRows) {
		var hasImports bool
		if checkErr := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_bot_imports WHERE ($1=domain OR $1 LIKE '%.'||domain))`, domain).Scan(&hasImports); checkErr != nil {
			return nil, checkErr
		}
		reason := "Немає імпорту журналу для цього домену."
		status := "not_available"
		if hasImports {
			status = "key_mismatch"
			reason = "Журнали для домену є, але вони не зіставляються з URL після ротації HMAC-ключа."
		}
		return []aiFetchLogEvidence{{Status: status, Explanation: reason}}, nil
	}
	if err != nil {
		return nil, err
	}
	fingerprint := fingerprintURL(s.cfg.TargetFingerprintKey, target)
	rows, err := s.pool.Query(ctx, `WITH recent_imports AS (
    SELECT id,domain FROM audit_bot_imports
    WHERE ($1=domain OR $1 LIKE '%.'||domain) AND fingerprint_key_id=$2
    ORDER BY created_at DESC,length(domain) DESC,id DESC LIMIT $3
)
SELECT STRING_AGG(DISTINCT i.domain, ', ' ORDER BY i.domain),e.bot,e.verification,COUNT(*),COUNT(*) FILTER(WHERE e.http_status>=400),
       ARRAY_AGG(DISTINCT e.http_status ORDER BY e.http_status),MIN(e.observed_at),MAX(e.observed_at),
       COUNT(*) FILTER(WHERE e.http_status IN (429,503))
FROM recent_imports i JOIN audit_bot_events e ON e.import_id=i.id
WHERE e.target_fingerprint=$4
GROUP BY e.bot,e.verification ORDER BY e.bot,e.verification`, domain, keyID, aiFetchRecentImports, fingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []aiFetchLogEvidence{}
	for rows.Next() {
		var item aiFetchLogEvidence
		var first, last time.Time
		var temporaryFailures int
		if err = rows.Scan(&item.ImportDomain, &item.Bot, &item.Verification, &item.Visits, &item.Failures, &item.HTTPStatuses, &first, &last, &temporaryFailures); err != nil {
			return nil, err
		}
		item.Status = "observed"
		item.First, item.Last = &first, &last
		item.Repeated429503 = temporaryFailures >= 2
		item.Explanation = "Фактичні HTTP-статуси з останніх сумісних імпортів журналу; User-Agent сам по собі не підтверджує особу бота."
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		items = append(items, aiFetchLogEvidence{Status: "no_matching_events", ImportDomain: importDomain, Explanation: "В останніх 20 сумісних імпортах немає подій із цим URL fingerprint; старіші імпорти не перевірялися."})
	}
	return items, nil
}
