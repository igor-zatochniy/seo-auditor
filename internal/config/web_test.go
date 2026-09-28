package config

import (
	"strings"
	"testing"
)

func TestLoadWeb(t *testing.T) {
	t.Setenv("WEB_ADDR", "")
	t.Setenv("MAX_WEB_URLS_PER_RUN", "")
	t.Setenv("WEB_ACCESS_TOKEN", strings.Repeat("a", 64))
	cfg, err := LoadWeb()
	if err != nil || cfg.Addr != "127.0.0.1:8080" || cfg.MaxURLs != 10000 {
		t.Fatalf("config=%+v error=%v", cfg, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"WEB_ADDR", "example.com:8080"}, {"WEB_ADDR", "127.0.0.1:0"},
		{"WEB_ACCESS_TOKEN", "short"}, {"WEB_ACCESS_TOKEN", strings.Repeat("a", 32) + ";"},
		{"WEB_ACCESS_TOKEN", strings.Repeat("a", 129)},
		{"MAX_WEB_URLS_PER_RUN", "0"}, {"MAX_WEB_URLS_PER_RUN", "10001"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadWeb(); err == nil {
				t.Fatal("accepted invalid web configuration")
			}
		})
	}
	t.Setenv("WEB_ADDR", ":8080")
	if _, err := LoadWeb(); err != nil {
		t.Fatal(err)
	}
}
