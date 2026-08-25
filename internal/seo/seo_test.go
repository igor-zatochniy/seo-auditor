package seo

import (
	"fmt"
	"strings"
	"testing"
)

func TestRobotsDirectiveSetBoundsRetainedStateAndPreservesPriority(t *testing.T) {
	var directives robotsDirectiveSet
	for index := range 10_000 {
		directives.Add(fmt.Sprintf("custom-directive-%d-%s", index, strings.Repeat("x", 32)))
	}
	directives.Add("noindex")

	value, truncated, originalRunes := directives.Result(StorageRobotsTagMaxRunes)
	if !truncated || originalRunes <= StorageRobotsTagMaxRunes {
		t.Fatalf("robots directives were not marked as truncated: truncated=%t original=%d", truncated, originalRunes)
	}
	if !strings.Contains(value, "noindex") {
		t.Fatalf("bounded robots directives lost noindex: %q", value)
	}
	for priority, bucket := range directives.buckets {
		if bucket.storedRunes > StorageRobotsTagMaxRunes {
			t.Fatalf(
				"priority %d retained %d runes, limit is %d",
				priority,
				bucket.storedRunes,
				StorageRobotsTagMaxRunes,
			)
		}
	}
}
