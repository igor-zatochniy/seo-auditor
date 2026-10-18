package botanalytics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func IsCDNFormat(s string) bool {
	return s == "cloudfront" || s == "cloudflare" || s == "vercel" || s == "fastly"
}

type edgeRow struct {
	URL, IP, UA, Method, Cache, ID string
	At                             time.Time
	Status                         int
	Origin                         *int
}

func cacheValue(raw string) string {
	v := strings.ToLower(raw)
	switch v {
	case "", "-":
		return ""
	case "hit", "miss", "bypass", "dynamic", "expired", "stale", "revalidated", "updating", "refreshhit", "error", "limitexceeded", "capacityexceeded", "redirect", "pass", "synth", "uncacheable", "prerender":
		return v
	}
	return "other"
}
func stringField(row map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(row[key], &v)
	return v
}
func intField(row map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := row[key]
	if !ok || string(raw) == "null" {
		return 0, false
	}
	n, e := strconv.Atoi(strings.Trim(string(raw), "\""))
	return n, e == nil
}
func timeField(raw json.RawMessage, unit string) (time.Time, error) {
	text := strings.Trim(string(raw), "\"")
	if t, e := time.Parse(time.RFC3339Nano, text); e == nil {
		return t, nil
	}
	n, e := strconv.ParseInt(text, 10, 64)
	if e != nil {
		return time.Time{}, errors.New("invalid timestamp")
	}
	switch unit {
	case "ns":
		return time.Unix(0, n).UTC(), nil
	case "ms":
		return time.UnixMilli(n).UTC(), nil
	}
	return time.Time{}, errors.New("RFC3339 timestamp required")
}
func optionalStatus(row map[string]json.RawMessage, key string) *int {
	n, ok := intField(row, key)
	if !ok || n < 100 || n > 599 {
		return nil
	}
	return &n
}

func parseEdgeJSON(line, format string) (edgeRow, error) {
	var root map[string]json.RawMessage
	var out edgeRow
	if err := json.Unmarshal([]byte(line), &root); err != nil || root == nil {
		return out, errors.New("JSONL object required")
	}
	var scheme, host, uri string
	var ok bool
	var err error
	switch format {
	case "cloudflare":
		scheme, host, uri = stringField(root, "ClientRequestScheme"), stringField(root, "ClientRequestHost"), stringField(root, "ClientRequestURI")
		out.IP, out.UA, out.Method = stringField(root, "ClientIP"), stringField(root, "ClientRequestUserAgent"), stringField(root, "ClientRequestMethod")
		out.Status, ok = intField(root, "EdgeResponseStatus")
		out.Origin = optionalStatus(root, "OriginResponseStatus")
		out.Cache = cacheValue(stringField(root, "CacheCacheStatus"))
		out.ID = stringField(root, "RayID")
		out.At, err = timeField(root["EdgeStartTimestamp"], "ns")
	case "vercel":
		var proxy map[string]json.RawMessage
		if json.Unmarshal(root["proxy"], &proxy) != nil || proxy == nil {
			return out, errors.New("proxy record required for vercel")
		}
		scheme, host, uri = stringField(proxy, "scheme"), stringField(proxy, "host"), stringField(proxy, "path")
		out.IP, out.Method = stringField(proxy, "clientIp"), stringField(proxy, "method")
		var agents []string
		if json.Unmarshal(proxy["userAgent"], &agents) == nil && len(agents) > 0 {
			out.UA = agents[0]
		} else {
			out.UA = stringField(proxy, "userAgent")
		}
		out.Status, ok = intField(proxy, "statusCode")
		out.Cache = cacheValue(stringField(proxy, "vercelCache"))
		out.ID = stringField(root, "requestId")
		out.At, err = timeField(proxy["timestamp"], "ms")
	case "fastly":
		out.URL = stringField(root, "url")
		out.IP, out.UA, out.Method = stringField(root, "client_ip"), stringField(root, "user_agent"), stringField(root, "method")
		out.Status, ok = intField(root, "status")
		out.Origin = optionalStatus(root, "origin_status")
		out.Cache = cacheValue(stringField(root, "cache_status"))
		out.ID = stringField(root, "request_id")
		out.At, err = timeField(root["timestamp"], "")
	}
	if format != "fastly" {
		if (scheme != "http" && scheme != "https") || host == "" || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
			return out, errors.New("scheme/host/path required")
		}
		out.URL = scheme + "://" + host + uri
	}
	if !ok || err != nil {
		return out, errors.New("status/timestamp required")
	}
	return out, nil
}

func parseCloudFront(line string, fields map[string]int) (edgeRow, error) {
	v := strings.Split(line, "\t")
	out := edgeRow{}
	get := func(key string) string {
		n, ok := fields[key]
		if !ok || n >= len(v) {
			return ""
		}
		return v[n]
	}
	host := get("x-host-header")
	if host == "" || host == "-" {
		host = get("cs(Host)")
	}
	scheme, stem := get("cs-protocol"), get("cs-uri-stem")
	if (scheme != "http" && scheme != "https") || host == "" || !strings.HasPrefix(stem, "/") || strings.HasPrefix(stem, "//") {
		return out, errors.New("missing CloudFront fields")
	}
	out.URL = scheme + "://" + host + stem
	if q := get("cs-uri-query"); q != "" && q != "-" {
		out.URL += "?" + q
	}
	var err error
	out.UA, err = url.PathUnescape(get("cs(User-Agent)"))
	if err != nil {
		return out, err
	}
	out.IP, out.Method, out.ID = get("c-ip"), get("cs-method"), get("x-edge-request-id")
	out.Cache = cacheValue(get("x-edge-response-result-type"))
	if out.Cache == "" {
		out.Cache = cacheValue(get("x-edge-result-type"))
	}
	out.Status, err = strconv.Atoi(get("sc-status"))
	if err != nil {
		return out, err
	}
	out.At, err = time.Parse(time.RFC3339, get("date")+"T"+get("time")+"Z")
	return out, err
}

func parseCDN(ctx context.Context, reader io.Reader, format, domain string, ranges map[string]Ranges, now time.Time) ([]Record, Stats, error) {
	stats := Stats{}
	records := []Record{}
	var err error
	domain, err = geo.NormalizeDomain(domain)
	if err != nil {
		return nil, stats, err
	}
	limited := &io.LimitedReader{R: reader, N: MaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 32768)
	fields := map[string]int{}
	seen := map[string]string{}
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return nil, stats, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if format == "cloudfront" && strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#Fields:") {
				fields = map[string]int{}
				for i, key := range strings.Fields(strings.TrimPrefix(line, "#Fields:")) {
					if _, exists := fields[key]; exists {
						return nil, stats, errors.New("повторні CloudFront fields")
					}
					fields[key] = i
				}
			}
			continue
		}
		stats.Lines++
		if stats.Lines > MaxLines {
			return nil, stats, errors.New("журнал перевищує 10 000 записів; усічення не виконується")
		}
		var row edgeRow
		if format == "cloudfront" {
			row, err = parseCloudFront(line, fields)
		} else {
			row, err = parseEdgeJSON(line, format)
		}
		if err != nil || row.At.IsZero() || row.At.After(now.Add(5*time.Minute)) || len(row.URL) > 2048 || len(row.UA) > 2048 || len(row.ID) > 256 {
			stats.Invalid++
			continue
		}
		if row.Status == 0 {
			stats.NoHTTP++
			continue
		}
		if row.Status < 100 || row.Status > 599 {
			stats.Invalid++
			continue
		}
		if row.Method != "GET" && row.Method != "HEAD" {
			stats.SkippedMethod++
			continue
		}
		bot := Classify(row.UA)
		if bot == "" {
			stats.NonBots++
			continue
		}
		normalized, e := crawler.NormalizeTargetURL(row.URL, false)
		if e != nil {
			stats.Invalid++
			continue
		}
		if !geo.InDomain(normalized, domain) {
			stats.ForeignDomain++
			continue
		}
		id := row.ID
		if id == "-" {
			id = ""
		}
		if id != "" {
			originStatus := 0
			if row.Origin != nil {
				originStatus = *row.Origin
			}
			signature := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%d\x00%s\x00%s", normalized, row.At.Format(time.RFC3339Nano), row.Status, row.UA, row.Cache, originStatus, row.Method, row.IP)
			if previous, exists := seen[id]; exists {
				if previous != signature {
					return nil, stats, errors.New("події з однаковим request ID містять суперечливі дані")
				}
				stats.Duplicates++
				continue
			}
			seen[id] = signature
		}
		ip, _ := netip.ParseAddr(row.IP)
		records = append(records, Record{URL: normalized, Bot: bot, Verification: Verify(ip, ranges[bot], now), Status: row.Status, At: row.At.UTC(), OriginStatus: row.Origin, CacheStatus: row.Cache, EventID: id})
		stats.Accepted++
	}
	if scanner.Err() != nil || limited.N <= 0 {
		return nil, stats, errors.New("файл перевищує 8 МіБ, рядок — 32 КіБ або читання перервано")
	}
	if stats.Lines == 0 || stats.Invalid == stats.Lines {
		return nil, stats, errors.New("немає коректних подій; перевірте формат і обов'язкові поля")
	}
	return records, stats, nil
}
