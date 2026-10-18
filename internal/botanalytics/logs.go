package botanalytics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

const MaxBytes = 8 << 20
const MaxLines = 10000
const RangeTTL = 7 * 24 * time.Hour

type Provider struct{ ID, Label, Source string }

var Providers = []Provider{
	{"googlebot", "Googlebot", "https://developers.google.com/static/crawling/ipranges/common-crawlers.json"},
	{"oai-searchbot", "OAI-SearchBot", "https://openai.com/searchbot.json"},
	{"gptbot", "GPTBot", "https://openai.com/gptbot.json"},
	{"chatgpt-user", "ChatGPT-User", "https://openai.com/chatgpt-user.json"},
}

type Ranges struct {
	Prefixes  []netip.Prefix
	FetchedAt time.Time
}
type Record struct {
	URL          string
	Bot          string
	Verification string
	Status       int
	At           time.Time
	OriginStatus *int
	CacheStatus  string
	EventID      string
}
type Stats struct {
	Lines         int `json:"lines"`
	Accepted      int `json:"accepted"`
	Invalid       int `json:"invalid"`
	NonBots       int `json:"non_bots"`
	ForeignDomain int `json:"foreign_domain"`
	Duplicates    int `json:"duplicates"`
	NoHTTP        int `json:"no_http_response"`
	SkippedMethod int `json:"skipped_method"`
}

func ParseRanges(raw []byte) ([]netip.Prefix, error) {
	if len(raw) > 512<<10 {
		return nil, errors.New("офіційний список IP перевищує 512 КіБ")
	}
	var value struct {
		Prefixes []struct {
			IPv4 string `json:"ipv4Prefix"`
			IPv6 string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || len(value.Prefixes) == 0 || len(value.Prefixes) > 4096 {
		return nil, errors.New("некоректний офіційний список IP")
	}
	result := make([]netip.Prefix, 0, len(value.Prefixes))
	for _, item := range value.Prefixes {
		raw := item.IPv4
		if raw == "" {
			raw = item.IPv6
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() < 8 || crawler.ValidateResolvedTargetIPs("bot-provider", []netip.Addr{prefix.Addr()}, false) != nil {
			return nil, errors.New("список містить некоректний, приватний або занадто широкий діапазон")
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

func Classify(ua string) string {
	match := botClaim.FindStringSubmatch(ua)
	if len(match) < 2 {
		return ""
	}
	id := strings.ToLower(match[1])
	if strings.HasPrefix(id, "googlebot-") {
		id = "googlebot"
	}
	return id
}

var botClaim = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_-])(googlebot(?:-image|-video)?|oai-searchbot|gptbot|chatgpt-user|claudebot)(?:[/; )]|$)`)

func Verify(ip netip.Addr, ranges Ranges, now time.Time) string {
	if !ip.IsValid() || len(ranges.Prefixes) == 0 || ranges.FetchedAt.IsZero() || now.Sub(ranges.FetchedAt) > RangeTTL || ranges.FetchedAt.After(now.Add(time.Minute)) {
		return "unverified"
	}
	for _, prefix := range ranges.Prefixes {
		if prefix.Contains(ip.Unmap()) {
			return "matched_range"
		}
	}
	return "range_mismatch"
}

var combined = regexp.MustCompile(`^(\S+) \S+ \S+ \[([^\]]+)\] "([^"]+)" (\d{3}) \S+ "[^"]*" "([^"]*)"(?: .*)?$`)

func Parse(ctx context.Context, reader io.Reader, format, domain string, ranges map[string]Ranges, now time.Time) ([]Record, Stats, error) {
	if IsCDNFormat(format) {
		return parseCDN(ctx, reader, format, domain, ranges, now)
	}
	stats := Stats{}
	var err error
	domain, err = geo.NormalizeDomain(domain)
	if err != nil {
		return nil, stats, err
	}
	if format != "jsonl" && format != "combined" {
		return nil, stats, errors.New("формат: JSONL або Nginx/Apache combined")
	}
	limited := &io.LimitedReader{R: reader, N: MaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 8192)
	records := make([]Record, 0, 256)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		stats.Lines++
		if stats.Lines > MaxLines {
			return nil, stats, errors.New("журнал перевищує 10 000 рядків; розділіть файл без перекриття періодів")
		}
		ip, at, request, ua, status, err := parseLine(line, format)
		if err != nil || status < 100 || status > 599 || at.IsZero() || at.After(now.Add(5*time.Minute)) {
			stats.Invalid++
			continue
		}
		bot := Classify(ua)
		if bot == "" {
			stats.NonBots++
			continue
		}
		if strings.Contains(request, " ") {
			parts := strings.Fields(request)
			if len(parts) != 3 || (parts[0] != "GET" && parts[0] != "HEAD") {
				stats.Invalid++
				continue
			}
			request = parts[1]
		}
		base, _ := url.Parse("https://" + domain + "/")
		u, err := url.Parse(request)
		if err != nil {
			stats.Invalid++
			continue
		}
		resolved := base.ResolveReference(u).String()
		if !geo.InDomain(resolved, domain) {
			stats.ForeignDomain++
			continue
		}
		normalized, err := crawler.NormalizeTargetURL(resolved, false)
		if err != nil {
			stats.Invalid++
			continue
		}
		records = append(records, Record{URL: normalized, Bot: bot, Verification: Verify(ip, ranges[bot], now), Status: status, At: at.UTC()})
		stats.Accepted++
	}
	if err := scanner.Err(); err != nil {
		return nil, stats, errors.New("рядок журналу перевищує 8 КіБ або файл не прочитано")
	}
	if limited.N <= 0 {
		return nil, stats, errors.New("журнал перевищує 8 МіБ")
	}
	if stats.Lines == 0 || stats.Invalid == stats.Lines {
		return nil, stats, errors.New("журнал не містить коректних рядків")
	}
	return records, stats, nil
}

func parseLine(line, format string) (netip.Addr, time.Time, string, string, int, error) {
	if format == "combined" {
		match := combined.FindStringSubmatch(line)
		if match == nil {
			return netip.Addr{}, time.Time{}, "", "", 0, errors.New("invalid line")
		}
		ip, _ := netip.ParseAddr(match[1])
		at, err := time.Parse("02/Jan/2006:15:04:05 -0700", match[2])
		status, _ := strconv.Atoi(match[4])
		return ip, at, match[3], match[5], status, err
	}
	var row struct {
		Time    string          `json:"time"`
		TimeISO string          `json:"time_iso8601"`
		IP      string          `json:"remote_addr"`
		Request string          `json:"request"`
		URI     string          `json:"request_uri"`
		UA      string          `json:"user_agent"`
		HTTPUA  string          `json:"http_user_agent"`
		Status  json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return netip.Addr{}, time.Time{}, "", "", 0, err
	}
	if row.Time == "" {
		row.Time = row.TimeISO
	}
	if row.Request == "" {
		row.Request = row.URI
	}
	if row.UA == "" {
		row.UA = row.HTTPUA
	}
	at, err := time.Parse(time.RFC3339, row.Time)
	ip, _ := netip.ParseAddr(row.IP)
	status, statusErr := strconv.Atoi(strings.Trim(string(row.Status), "\""))
	if statusErr != nil || row.Request == "" || len(row.UA) > 2048 {
		return ip, at, "", "", 0, errors.New("invalid row")
	}
	return ip, at, row.Request, row.UA, status, err
}
