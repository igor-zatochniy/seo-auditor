package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAnswerStorageRedactsAndBoundsUTF8(t *testing.T) {
	s := boundedAnswerText("Джерело https://example.com/?token=runtime-private-value "+strings.Repeat("ї", 1000), 101)
	if !utf8.ValidString(s) || len(s) > 101 || strings.Contains(s, "runtime-private-value") {
		t.Fatal(s)
	}
}
