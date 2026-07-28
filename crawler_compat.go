package main

import (
	"net/http"
	"net/netip"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
)

func newHTTPTransport(cfg Config, attemptTimeout time.Duration) *http.Transport {
	return crawler.NewHTTPTransport(crawler.TransportConfig{
		Workers:             cfg.Workers,
		AllowPrivateTargets: cfg.AllowPrivateTargets,
	}, attemptTimeout)
}

func newRobotsHTTPClient(transport http.RoundTripper) *http.Client {
	return crawler.NewRobotsHTTPClient(transport, MaxRobotsRedirects)
}

func validateResolvedTargetIPs(host string, ips []netip.Addr, allowPrivateTargets bool) error {
	return crawler.ValidateResolvedTargetIPs(host, ips, allowPrivateTargets)
}

func normalizeTargetURL(rawURL string, allowPrivateTargets bool) (string, error) {
	return crawler.NormalizeTargetURL(rawURL, allowPrivateTargets)
}
