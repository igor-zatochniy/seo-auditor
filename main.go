package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

const (
	MinTitleLen       = 40
	MaxTitleLen       = 65
	MinDescriptionLen = 120
	MaxDescriptionLen = 170
	UserAgentStr      = "Go-SEOParser-Bot/1.0"

	DefaultWorkers          = 3
	MaxWorkers              = 32
	DefaultMaxHTMLBodyBytes = int64(5 * 1024 * 1024)
	MaxRobotsBodyBytes      = int64(512 * 1024)
)

type Config struct {
	Workers             int
	DatabaseURL         string
	HTTPRequestTimeout  time.Duration
	RobotsTimeout       time.Duration
	DBConnectTimeout    time.Duration
	DBFetchTimeout      time.Duration
	DBWriteTimeout      time.Duration
	MaxHTMLBodyBytes    int64
	AllowPrivateTargets bool
	RateLimitInterval   time.Duration
}

// SEOData contains the full set of metrics collected by the parser.
type SEOData struct {
	URL                string
	StatusCode         int
	IsRedirect         bool
	RedirectURL        string
	Title              string
	TitleStatus        string
	Description        string
	DescriptionStatus  string
	H1                 string
	H1Count            int
	H2ToH6Status       string
	OGTitle            string
	OGDescription      string
	OGImage            string
	TwitterCard        string
	InternalLinksCount int
	ExternalLinksCount int
	LinksCount         int
	CanonicalURL       string
	IsSelfCanonical    bool
	MetaRobots         string
	XRobotsTag         string
	RobotsAllowed      bool
	HasJsonLd          bool
	HasViewport        bool
	TotalImages        int
	ImagesMissingAlt   int
	WordCount          int
	Duration           time.Duration
}

type Result struct {
	Data  SEOData
	Error error
}

type robotsRule struct {
	allow       bool
	pattern     string
	specificity int
}

type robotsGroup struct {
	agents []string
	rules  []robotsRule
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	slog.Info("Запускається етичний SEO-аудитор")
	cfg := loadConfig()

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info(
		"Конфігурацію рантайму ініціалізовано",
		"workers",
		cfg.Workers,
		"request_timeout",
		cfg.HTTPRequestTimeout.String(),
		"max_html_body_bytes",
		cfg.MaxHTMLBodyBytes,
	)

	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("Не вдалося розібрати рядок підключення до PostgreSQL", "error", err)
		return
	}

	poolConfig.MaxConns = int32(cfg.Workers + 2)
	poolConfig.MinConns = 2
	if poolConfig.MinConns > poolConfig.MaxConns {
		poolConfig.MinConns = poolConfig.MaxConns
	}
	poolConfig.MaxConnIdleTime = 15 * time.Minute
	poolConfig.MaxConnLifetime = 1 * time.Hour

	dbInitCtx, dbCancel := context.WithTimeout(rootCtx, cfg.DBConnectTimeout)
	dbPool, err := pgxpool.NewWithConfig(dbInitCtx, poolConfig)
	if err != nil {
		dbCancel()
		slog.Error("Не вдалося ініціалізувати пул підключень PostgreSQL", "error", err)
		return
	}

	if err := dbPool.Ping(dbInitCtx); err != nil {
		dbCancel()
		dbPool.Close()
		slog.Error("PostgreSQL недоступний під час перевірки підключення", "error", err)
		return
	}
	dbCancel()
	slog.Info("Підключення до PostgreSQL підтверджено", "max_conns", poolConfig.MaxConns)

	defer func() {
		slog.Info("Закривається пул підключень PostgreSQL")
		dbPool.Close()
	}()

	urls, err := fetchTargetURLs(rootCtx, dbPool, cfg)
	if err != nil {
		slog.Error("Не вдалося отримати чергу URL", "error", err)
		return
	}
	if len(urls) == 0 {
		slog.Warn("Черга URL порожня")
		return
	}
	slog.Info("Чергу URL сформовано", "total_urls", len(urls))

	limiter := rate.NewLimiter(rate.Every(cfg.RateLimitInterval), cfg.Workers)

	jobs := make(chan string, len(urls))
	results := make(chan Result, len(urls))

	customTransport := &http.Transport{
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    cfg.Workers,
		MaxConnsPerHost:        cfg.Workers * 2,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  cfg.HTTPRequestTimeout,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}

	httpClient := &http.Client{
		Transport: customTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer customTransport.CloseIdleConnections()

	var wg sync.WaitGroup
	for w := 1; w <= cfg.Workers; w++ {
		wg.Add(1)
		go worker(rootCtx, w, jobs, results, limiter, httpClient, cfg, &wg)
	}

	go func() {
	sendLoop:
		for _, targetURL := range urls {
			select {
			case <-rootCtx.Done():
				slog.Warn("Отримано сигнал зупинки, нові задачі більше не плануються")
				break sendLoop
			case jobs <- targetURL:
			}
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	slog.Info("Починається паралельна обробка URL та збереження результатів")
	saveResults(rootCtx, dbPool, results, cfg)
	slog.Info("Роботу парсера завершено")
}

func loadConfig() Config {
	workers := intFromEnv("WORKERS", DefaultWorkers, 1, MaxWorkers)
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		databaseURL = fmt.Sprintf(
			"postgres://localhost:5432/seo_db?sslmode=disable&pool_max_conns=%d",
			workers+2,
		)
		slog.Warn("DATABASE_URL не задано, використовується локальне значення за замовчуванням")
	}

	return Config{
		Workers:             workers,
		DatabaseURL:         databaseURL,
		HTTPRequestTimeout:  durationFromEnv("HTTP_REQUEST_TIMEOUT", 5*time.Second),
		RobotsTimeout:       durationFromEnv("ROBOTS_TIMEOUT", 3*time.Second),
		DBConnectTimeout:    durationFromEnv("DB_CONNECT_TIMEOUT", 5*time.Second),
		DBFetchTimeout:      durationFromEnv("DB_FETCH_TIMEOUT", 5*time.Second),
		DBWriteTimeout:      durationFromEnv("DB_WRITE_TIMEOUT", 3*time.Second),
		MaxHTMLBodyBytes:    int64FromEnv("MAX_HTML_BODY_BYTES", DefaultMaxHTMLBodyBytes, 1024, 50*1024*1024),
		AllowPrivateTargets: boolFromEnv("ALLOW_PRIVATE_TARGETS", false),
		RateLimitInterval:   durationFromEnv("RATE_LIMIT_INTERVAL", 500*time.Millisecond),
	}
}

func intFromEnv(name string, fallback, minValue, maxValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		slog.Warn(
			"Значення змінної середовища некоректне, використовується значення за замовчуванням",
			"name",
			name,
			"value",
			raw,
			"default",
			fallback,
		)
		return fallback
	}

	return value
}

func int64FromEnv(name string, fallback, minValue, maxValue int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minValue || value > maxValue {
		slog.Warn(
			"Значення змінної середовища некоректне, використовується значення за замовчуванням",
			"name",
			name,
			"value",
			raw,
			"default",
			fallback,
		)
		return fallback
	}

	return value
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		slog.Warn(
			"Тривалість у змінній середовища некоректна, використовується значення за замовчуванням",
			"name",
			name,
			"value",
			raw,
			"default",
			fallback.String(),
		)
		return fallback
	}

	return value
}

func boolFromEnv(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn(
			"Булеве значення змінної середовища некоректне, використовується значення за замовчуванням",
			"name",
			name,
			"value",
			raw,
			"default",
			fallback,
		)
		return fallback
	}

	return value
}

func fetchTargetURLs(ctx context.Context, dbPool *pgxpool.Pool, cfg Config) ([]string, error) {
	slog.Info("Завантажується черга URL з PostgreSQL")

	dbFetchCtx, fetchCancel := context.WithTimeout(ctx, cfg.DBFetchTimeout)
	defer fetchCancel()

	rows, err := dbPool.Query(dbFetchCtx, "SELECT url FROM pages_to_scan WHERE is_active = TRUE")
	if err != nil {
		return nil, fmt.Errorf("query active pages: %w", err)
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var rawURL string
		if err := rows.Scan(&rawURL); err != nil {
			slog.Error("Не вдалося прочитати URL з рядка PostgreSQL", "error", err)
			continue
		}

		normalizedURL, err := normalizeTargetURL(rawURL, cfg.AllowPrivateTargets)
		if err != nil {
			slog.Warn("URL пропущено через помилку валідації", "url", rawURL, "error", err)
			continue
		}
		urls = append(urls, normalizedURL)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active pages: %w", err)
	}

	return urls, nil
}

func saveResults(ctx context.Context, dbPool *pgxpool.Pool, results <-chan Result, cfg Config) {
	query := `
		INSERT INTO seo_results (
			url, status_code, is_redirect, redirect_url, title, title_status, description, description_status,
			h1, h1_count, h2_to_h6_status, og_title, og_description, og_image, twitter_card,
			internal_links_count, external_links_count, links_count, canonical_url, is_self_canonical,
			meta_robots, x_robots_tag, robots_allowed, has_json_ld, has_viewport,
			total_images, images_missing_alt, word_count, duration_ms
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (url) DO UPDATE SET
			status_code = EXCLUDED.status_code,
			is_redirect = EXCLUDED.is_redirect,
			redirect_url = EXCLUDED.redirect_url,
			title = EXCLUDED.title,
			title_status = EXCLUDED.title_status,
			description = EXCLUDED.description,
			description_status = EXCLUDED.description_status,
			h1 = EXCLUDED.h1,
			h1_count = EXCLUDED.h1_count,
			h2_to_h6_status = EXCLUDED.h2_to_h6_status,
			og_title = EXCLUDED.og_title,
			og_description = EXCLUDED.og_description,
			og_image = EXCLUDED.og_image,
			twitter_card = EXCLUDED.twitter_card,
			internal_links_count = EXCLUDED.internal_links_count,
			external_links_count = EXCLUDED.external_links_count,
			links_count = EXCLUDED.links_count,
			canonical_url = EXCLUDED.canonical_url,
			is_self_canonical = EXCLUDED.is_self_canonical,
			meta_robots = EXCLUDED.meta_robots,
			x_robots_tag = EXCLUDED.x_robots_tag,
			robots_allowed = EXCLUDED.robots_allowed,
			has_json_ld = EXCLUDED.has_json_ld,
			has_viewport = EXCLUDED.has_viewport,
			total_images = EXCLUDED.total_images,
			images_missing_alt = EXCLUDED.images_missing_alt,
			word_count = EXCLUDED.word_count,
			duration_ms = EXCLUDED.duration_ms,
			created_at = CURRENT_TIMESTAMP;`

	for res := range results {
		if res.Error != nil {
			slog.Error("Задача завершилася помилкою", "error", res.Error)
			continue
		}

		d := res.Data
		dbWriteCtx, writeCancel := context.WithTimeout(ctx, cfg.DBWriteTimeout)
		_, err := dbPool.Exec(
			dbWriteCtx,
			query,
			d.URL,
			d.StatusCode,
			d.IsRedirect,
			d.RedirectURL,
			d.Title,
			d.TitleStatus,
			d.Description,
			d.DescriptionStatus,
			d.H1,
			d.H1Count,
			d.H2ToH6Status,
			d.OGTitle,
			d.OGDescription,
			d.OGImage,
			d.TwitterCard,
			d.InternalLinksCount,
			d.ExternalLinksCount,
			d.LinksCount,
			d.CanonicalURL,
			d.IsSelfCanonical,
			d.MetaRobots,
			d.XRobotsTag,
			d.RobotsAllowed,
			d.HasJsonLd,
			d.HasViewport,
			d.TotalImages,
			d.ImagesMissingAlt,
			d.WordCount,
			d.Duration.Milliseconds(),
		)
		writeCancel()

		if err != nil {
			slog.Error("Не вдалося зберегти результат SEO-аудиту", "url", d.URL, "error", err)
			continue
		}

		slog.Debug("Результат SEO-аудиту збережено", "url", d.URL)
	}
}

func worker(
	ctx context.Context,
	id int,
	jobs <-chan string,
	results chan<- Result,
	limiter *rate.Limiter,
	client *http.Client,
	cfg Config,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	workerLogger := slog.With("worker_id", id)

	for targetURL := range jobs {
		select {
		case <-ctx.Done():
			workerLogger.Debug("Worker зупиняється за сигналом контексту")
			return
		default:
		}

		if err := limiter.Wait(ctx); err != nil {
			workerLogger.Debug("Лімітер швидкості зупинено", "error", err)
			return
		}

		start := time.Now()

		allowed := isAllowedByRobots(ctx, client, targetURL, cfg.RobotsTimeout)
		if !allowed {
			workerLogger.Warn("Сканування URL заборонено правилами robots.txt", "url", targetURL)
			results <- Result{Data: SEOData{URL: targetURL, RobotsAllowed: false, StatusCode: http.StatusForbidden, Duration: time.Since(start)}}
			continue
		}

		reqCtx, reqCancel := context.WithTimeout(ctx, cfg.HTTPRequestTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
		if err != nil {
			reqCancel()
			results <- Result{Error: fmt.Errorf("worker %d cannot create request for %s: %w", id, targetURL, err)}
			continue
		}
		req.Header.Set("User-Agent", UserAgentStr)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")

		resp, err := client.Do(req)
		if err != nil {
			reqCancel()
			results <- Result{Error: fmt.Errorf("worker %d network request failed for %s: %w", id, targetURL, err)}
			continue
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			redirectURL := resp.Header.Get("Location")
			resp.Body.Close()
			reqCancel()

			workerLogger.Info(
				"Виявлено HTTP-редирект",
				"from",
				targetURL,
				"to",
				redirectURL,
				"status",
				resp.StatusCode,
			)

			results <- Result{Data: SEOData{
				URL:           targetURL,
				StatusCode:    resp.StatusCode,
				IsRedirect:    true,
				RedirectURL:   redirectURL,
				RobotsAllowed: true,
				Duration:      time.Since(start),
			}}
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			resp.Body.Close()
			reqCancel()
			results <- Result{Error: fmt.Errorf("worker %d rejected %s: missing Content-Type header", id, targetURL)}
			continue
		}

		contentTypeLower := strings.ToLower(contentType)
		if !strings.Contains(contentTypeLower, "text/html") &&
			!strings.Contains(contentTypeLower, "application/xhtml+xml") {
			resp.Body.Close()
			reqCancel()
			workerLogger.Warn("Пропущено непідтримуваний тип контенту", "url", targetURL, "content_type", contentType)
			results <- Result{Error: fmt.Errorf("worker %d skipped unsupported content type %q for %s", id, contentType, targetURL)}
			continue
		}

		data, err := parsePage(resp, targetURL, cfg.MaxHTMLBodyBytes)
		resp.Body.Close()
		reqCancel()

		if err != nil {
			results <- Result{Error: fmt.Errorf("worker %d cannot parse HTML for %s: %w", id, targetURL, err)}
			continue
		}

		data.RobotsAllowed = true
		data.Duration = time.Since(start)

		select {
		case <-ctx.Done():
			workerLogger.Warn("Відправлення результату скасовано через зупинку", "url", targetURL)
			return
		case results <- Result{Data: data}:
		}
	}
}

func normalizeTargetURL(rawURL string, allowPrivateTargets bool) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty URL")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("credentials in URL are not allowed")
	}
	if !allowPrivateTargets && isPrivateHost(parsed.Host) {
		return "", fmt.Errorf("private or local targets are disabled")
	}

	return parsed.String(), nil
}

func isPrivateHost(host string) bool {
	hostOnly := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = parsedHost
	}

	hostOnly = strings.Trim(strings.ToLower(strings.TrimSuffix(hostOnly, ".")), "[]")
	if hostOnly == "localhost" || strings.HasSuffix(hostOnly, ".localhost") {
		return true
	}

	ip, err := netip.ParseAddr(hostOnly)
	if err != nil {
		return false
	}

	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

func isAllowedByRobots(ctx context.Context, client *http.Client, targetURL string, timeout time.Duration) bool {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false
	}

	robotsURL := fmt.Sprintf("%s://%s/robots.txt", parsed.Scheme, parsed.Host)

	robotsCtx, robotsCancel := context.WithTimeout(ctx, timeout)
	defer robotsCancel()

	req, err := http.NewRequestWithContext(robotsCtx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return true
	}
	req.Header.Set("User-Agent", UserAgentStr)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("Не вдалося прочитати robots.txt, хост вважається дозволеним", "url", robotsURL, "error", err)
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return true
	}

	body, err := readLimited(resp.Body, MaxRobotsBodyBytes)
	if err != nil {
		slog.Warn("robots.txt перевищує дозволений розмір або не може бути прочитаний", "url", robotsURL, "error", err)
		return true
	}

	return isPathAllowedByRobots(string(body), UserAgentStr, robotsRequestPath(parsed))
}

func robotsRequestPath(parsed *url.URL) string {
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return path
}

func isPathAllowedByRobots(content, userAgent, requestPath string) bool {
	groups := parseRobotsGroups(content)
	group, ok := selectRobotsGroup(groups, userAgent)
	if !ok || len(group.rules) == 0 {
		return true
	}

	allowed := true
	bestSpecificity := -1
	for _, rule := range group.rules {
		if !robotsPatternMatches(rule.pattern, requestPath) {
			continue
		}
		if rule.specificity > bestSpecificity || (rule.specificity == bestSpecificity && rule.allow) {
			allowed = rule.allow
			bestSpecificity = rule.specificity
		}
	}

	return allowed
}

func parseRobotsGroups(content string) []robotsGroup {
	var groups []robotsGroup
	var current robotsGroup
	seenRule := false

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(stripRobotsComment(line))
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if len(current.agents) > 0 && seenRule {
				groups = append(groups, current)
				current = robotsGroup{}
				seenRule = false
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "allow", "disallow":
			if len(current.agents) == 0 {
				continue
			}
			seenRule = true
			if key == "disallow" && value == "" {
				continue
			}
			current.rules = append(current.rules, robotsRule{
				allow:       key == "allow",
				pattern:     value,
				specificity: robotsRuleSpecificity(value),
			})
		}
	}

	if len(current.agents) > 0 {
		groups = append(groups, current)
	}

	return groups
}

func stripRobotsComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

func selectRobotsGroup(groups []robotsGroup, userAgent string) (robotsGroup, bool) {
	bestLength := -1
	var selected robotsGroup
	found := false

	for _, group := range groups {
		groupBestLength := -1
		for _, agent := range group.agents {
			if robotsAgentMatches(agent, userAgent) && len(agent) > groupBestLength {
				groupBestLength = len(agent)
			}
		}

		if groupBestLength < 0 {
			continue
		}
		if groupBestLength > bestLength {
			selected = group
			bestLength = groupBestLength
			found = true
			continue
		}
		if groupBestLength == bestLength {
			selected.rules = append(selected.rules, group.rules...)
		}
	}

	return selected, found
}

func robotsAgentMatches(agent, userAgent string) bool {
	agent = strings.TrimSpace(strings.ToLower(agent))
	if agent == "" {
		return false
	}
	if agent == "*" {
		return true
	}

	ua := strings.ToLower(userAgent)
	product := strings.SplitN(ua, "/", 2)[0]
	return strings.Contains(ua, agent) || strings.Contains(product, agent)
}

func robotsRuleSpecificity(pattern string) int {
	cleaned := strings.NewReplacer("*", "", "$", "").Replace(pattern)
	return len(cleaned)
}

func robotsPatternMatches(pattern, requestPath string) bool {
	if pattern == "" {
		return false
	}

	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}

	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
	if endAnchored {
		expr += "$"
	}

	matched, err := regexp.MatchString(expr, requestPath)
	return err == nil && matched
}

func parsePage(resp *http.Response, targetURL string, maxBodyBytes int64) (SEOData, error) {
	data := SEOData{
		URL:        targetURL,
		StatusCode: resp.StatusCode,
		XRobotsTag: strings.TrimSpace(resp.Header.Get("X-Robots-Tag")),
	}

	if resp.StatusCode != http.StatusOK {
		return data, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := readLimited(resp.Body, maxBodyBytes)
	if err != nil {
		return data, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return data, fmt.Errorf("parse HTML: %w", err)
	}

	data.Title = strings.TrimSpace(doc.Find("title").First().Text())
	titleLen := utf8.RuneCountInString(data.Title)
	if titleLen == 0 {
		data.TitleStatus = "Missing"
	} else if titleLen < MinTitleLen {
		data.TitleStatus = "Too Short"
	} else if titleLen > MaxTitleLen {
		data.TitleStatus = "Too Long"
	} else {
		data.TitleStatus = "OK"
	}

	data.Description = strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	descLen := utf8.RuneCountInString(data.Description)
	if descLen == 0 {
		data.DescriptionStatus = "Missing"
	} else if descLen < MinDescriptionLen {
		data.DescriptionStatus = "Too Short"
	} else if descLen > MaxDescriptionLen {
		data.DescriptionStatus = "Too Long"
	} else {
		data.DescriptionStatus = "OK"
	}

	h1Selection := doc.Find("h1")
	data.H1Count = h1Selection.Length()
	if data.H1Count > 0 {
		data.H1 = strings.TrimSpace(h1Selection.First().Text())
	} else {
		data.H1 = "[Missing H1]"
	}

	var subHeaders []string
	for i := 2; i <= 6; i++ {
		count := doc.Find(fmt.Sprintf("h%d", i)).Length()
		if count > 0 {
			subHeaders = append(subHeaders, fmt.Sprintf("H%d:%d", i, count))
		}
	}
	if len(subHeaders) > 0 {
		data.H2ToH6Status = strings.Join(subHeaders, ", ")
	} else {
		data.H2ToH6Status = "No sub-headers (H2-H6)"
	}

	data.OGTitle = strings.TrimSpace(doc.Find("meta[property='og:title']").AttrOr("content", ""))
	data.OGDescription = strings.TrimSpace(doc.Find("meta[property='og:description']").AttrOr("content", ""))
	data.OGImage = strings.TrimSpace(doc.Find("meta[property='og:image']").AttrOr("content", ""))
	data.TwitterCard = strings.TrimSpace(doc.Find("meta[name='twitter:card']").AttrOr("content", ""))

	baseParsed, _ := url.Parse(targetURL)
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}
		href = strings.TrimSpace(href)
		hrefLower := strings.ToLower(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(hrefLower, "javascript:") || strings.HasPrefix(hrefLower, "mailto:") || strings.HasPrefix(hrefLower, "tel:") {
			return
		}

		linkParsed, err := url.Parse(href)
		if err != nil {
			return
		}

		if linkParsed.Host == "" || strings.EqualFold(linkParsed.Host, baseParsed.Host) {
			data.InternalLinksCount++
		} else {
			data.ExternalLinksCount++
		}
	})
	data.LinksCount = data.InternalLinksCount + data.ExternalLinksCount

	data.CanonicalURL = strings.TrimSpace(doc.Find("link[rel='canonical']").AttrOr("href", ""))
	data.IsSelfCanonical = isSelfCanonical(data.CanonicalURL, targetURL)
	data.MetaRobots = strings.TrimSpace(doc.Find("meta[name='robots']").AttrOr("content", ""))

	data.HasJsonLd = doc.Find("script[type='application/ld+json']").Length() > 0
	data.HasViewport = doc.Find("meta[name='viewport']").Length() > 0

	imgSelection := doc.Find("img")
	data.TotalImages = imgSelection.Length()
	imgSelection.Each(func(i int, s *goquery.Selection) {
		alt, exists := s.Attr("alt")
		if !exists || strings.TrimSpace(alt) == "" {
			data.ImagesMissingAlt++
		}
	})

	bodyText := doc.Find("body").Text()
	words := strings.Fields(bodyText)
	data.WordCount = len(words)

	return data, nil
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive")
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds configured limit of %d bytes", maxBytes)
	}

	return body, nil
}

func isSelfCanonical(canonicalURL, targetURL string) bool {
	if strings.TrimSpace(canonicalURL) == "" {
		return true
	}

	targetParsed, err := url.Parse(targetURL)
	if err != nil {
		return false
	}

	canonicalParsed, err := url.Parse(canonicalURL)
	if err != nil {
		return false
	}
	if !canonicalParsed.IsAbs() {
		canonicalParsed = targetParsed.ResolveReference(canonicalParsed)
	}

	normalize := func(parsed *url.URL) string {
		copyValue := *parsed
		copyValue.Scheme = strings.ToLower(copyValue.Scheme)
		copyValue.Host = strings.ToLower(copyValue.Host)
		copyValue.Fragment = ""
		return strings.TrimRight(copyValue.String(), "/")
	}

	return normalize(canonicalParsed) == normalize(targetParsed)
}
