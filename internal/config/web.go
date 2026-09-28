package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type WebConfig struct {
	Addr        string
	MaxURLs     int
	AccessToken string
}

func LoadWeb() (WebConfig, error) {
	addr := strings.TrimSpace(os.Getenv("WEB_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return WebConfig{}, fmt.Errorf("WEB_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return WebConfig{}, fmt.Errorf("invalid WEB_ADDR port")
	}
	if host != "" && host != "0.0.0.0" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return WebConfig{}, fmt.Errorf("WEB_ADDR must bind loopback, or :port inside a loopback-published container")
	}
	token := strings.TrimSpace(os.Getenv("WEB_ACCESS_TOKEN"))
	if len(token) < 32 || len(token) > 128 || strings.ContainsFunc(token, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) {
		return WebConfig{}, fmt.Errorf("WEB_ACCESS_TOKEN must contain 32-128 URL-safe ASCII characters")
	}
	limit, err := intFromEnv("MAX_WEB_URLS_PER_RUN", 10000, 1, 10000)
	return WebConfig{Addr: addr, MaxURLs: limit, AccessToken: token}, err
}
