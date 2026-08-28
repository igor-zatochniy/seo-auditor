package crawler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

type TransportConfig struct {
	Workers             int
	AllowPrivateTargets bool
}

// NormalizeHostname converts equivalent DNS representations to one comparison key.
func NormalizeHostname(host string) (string, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return "", fmt.Errorf("empty hostname")
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		return strings.ToLower(ip.Unmap().String()), nil
	}

	asciiHost, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("normalize hostname %q: %w", host, err)
	}
	return strings.ToLower(strings.TrimSuffix(asciiHost, ".")), nil
}

// NormalizeAuthority canonicalizes a URL authority without changing the URL shown to users.
func NormalizeAuthority(parsed *url.URL) (string, error) {
	if parsed == nil {
		return "", fmt.Errorf("URL is required")
	}
	host, err := NormalizeHostname(parsed.Hostname())
	if err != nil {
		return "", err
	}

	port := parsed.Port()
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		if port == "80" {
			port = ""
		}
	case "https":
		if port == "443" {
			port = ""
		}
	}
	if port != "" {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}

var blockedTargetIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var (
	nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalUsePrefix  = netip.MustParsePrefix("64:ff9b:1::/48")
)

func NewHTTPTransport(cfg TransportConfig, attemptTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   attemptTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext:           SafeDialContext(dialer, cfg.AllowPrivateTargets),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.Workers,
		MaxConnsPerHost:       cfg.Workers * 2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: attemptTimeout,
		ExpectContinueTimeout: 1 * time.Second,

		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
}

func NewRobotsHTTPClient(transport http.RoundTripper, maxRedirects int) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func SafeDialContext(dialer *net.Dialer, allowPrivateTargets bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split dial address %q: %w", address, err)
		}

		ips, err := resolveDialIPs(ctx, network, host)
		if err != nil {
			return nil, err
		}
		if err := ValidateResolvedTargetIPs(host, ips, allowPrivateTargets); err != nil {
			return nil, err
		}

		var lastErr error
		for _, ip := range ips {
			if !ipMatchesNetwork(ip, network) {
				continue
			}

			dialAddress := net.JoinHostPort(ip.String(), port)
			conn, err := dialer.DialContext(ctx, network, dialAddress)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}

		if lastErr != nil {
			return nil, fmt.Errorf("dial %s: %w", address, lastErr)
		}
		return nil, fmt.Errorf("no resolved IP address for %s matches network %s", address, network)
	}
}

func NormalizeTargetURL(rawURL string, allowPrivateTargets bool) (string, error) {
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
	if !allowPrivateTargets && IsPrivateHost(parsed.Host) {
		return "", fmt.Errorf("private or local targets are disabled")
	}
	parsed.Fragment = ""

	return parsed.String(), nil
}

func IsPrivateHost(host string) bool {
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

	return IsBlockedTargetIP(ip)
}

func IsBlockedTargetIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if nat64LocalUsePrefix.Contains(ip) {
		return true
	}
	if embeddedIPv4, ok := wellKnownNAT64IPv4(ip); ok {
		return IsBlockedTargetIP(embeddedIPv4)
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}

	for _, prefix := range blockedTargetIPPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}

	return false
}

func wellKnownNAT64IPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() || !nat64WellKnownPrefix.Contains(ip) {
		return netip.Addr{}, false
	}

	octets := ip.As16()
	return netip.AddrFrom4([4]byte{octets[12], octets[13], octets[14], octets[15]}), true
}

func ValidateResolvedTargetIPs(host string, ips []netip.Addr, allowPrivateTargets bool) error {
	if len(ips) == 0 {
		return fmt.Errorf("resolve %s: no IP addresses returned", host)
	}
	if allowPrivateTargets {
		return nil
	}

	for _, ip := range ips {
		if IsBlockedTargetIP(ip) {
			return fmt.Errorf("private or local network target is disabled: %s resolved to %s", host, ip)
		}
	}

	return nil
}

func resolveDialIPs(ctx context.Context, network, host string) ([]netip.Addr, error) {
	normalizedHost := strings.Trim(strings.TrimSpace(strings.TrimSuffix(host, ".")), "[]")
	if normalizedHost == "" {
		return nil, fmt.Errorf("empty dial host")
	}

	if ip, err := netip.ParseAddr(normalizedHost); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}

	lookupNetwork := "ip"
	if strings.HasSuffix(network, "4") {
		lookupNetwork = "ip4"
	} else if strings.HasSuffix(network, "6") {
		lookupNetwork = "ip6"
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, lookupNetwork, normalizedHost)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no IP addresses returned", normalizedHost)
	}

	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	return ips, nil
}

func ipMatchesNetwork(ip netip.Addr, network string) bool {
	ip = ip.Unmap()
	if strings.HasSuffix(network, "4") {
		return ip.Is4()
	}
	if strings.HasSuffix(network, "6") {
		return ip.Is6()
	}
	return true
}
