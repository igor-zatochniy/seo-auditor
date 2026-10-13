package render

import (
	"strings"
	"testing"
)

func TestResourceFailuresAreBounded(t *testing.T) {
	var out Output
	for i := 0; i < MaxResourceIssues+1; i++ {
		resourceIssue(&out, "https://example.com/x", "Script", &ResourceFailure{Code: "http_status", HTTP: 404})
	}
	if len(out.Resources) != MaxResourceIssues || !out.ResourcesTruncated || out.Resources[0].HTTP != 404 {
		t.Fatal("resource budget missing")
	}
	var long Output
	resourceIssue(&long, strings.Repeat("x", 2049), "Script", &ResourceFailure{Code: "invalid_target"})
	if !long.Resources[0].URLTruncated || len(long.Resources[0].URL) > 2048 {
		t.Fatal("unbounded resource URL")
	}
}
