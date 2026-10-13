package main

import (
	"strings"
	"testing"
	"time"
)

func TestAdaptiveIntervalBoundsAndPriorities(t *testing.T) {
	for stable := 0; stable < 100; stable++ {
		for priority := 1; priority <= 3; priority++ {
			d := adaptiveInterval(2, 168, priority, stable, false)
			if d < 2*time.Hour || d > 168*time.Hour {
				t.Fatalf("outside bounds: %v", d)
			}
		}
	}
	if adaptiveInterval(2, 168, 1, 3, false) <= adaptiveInterval(2, 168, 3, 3, false) {
		t.Fatal("priorities do not affect cadence")
	}
	if adaptiveInterval(2, 168, 1, 8, true) != 2*time.Hour {
		t.Fatal("failures not rechecked at minimum interval")
	}
}

func TestConditionalValidatorsRejectControlBytes(t *testing.T) {
	for _, value := range []string{"header\r\ninjected: bad", "null\x00value", "кирилиця"} {
		if safeHTTPValidator(value, 1024) != "" {
			t.Fatalf("invalid validator accepted: %q", value)
		}
	}
	if safeHTTPValidator(`"revision-1"`, 1024) != `"revision-1"` {
		t.Fatal("valid ETag changed")
	}
}

func TestHTTPValidatorsAreEncryptedAndAuthenticated(t *testing.T) {
	key := []byte("local-development-only-fingerprint-key")
	sealed, err := sealValidator(key, `"signed-fixture"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "signed-fixture") {
		t.Fatal("validator persisted in plaintext")
	}
	opened, err := openValidator(key, sealed)
	if err != nil || opened != `"signed-fixture"` {
		t.Fatalf("round trip failed: %q %v", opened, err)
	}
	if _, err = openValidator([]byte("different-local-development-only-key"), sealed); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err = openValidator(key, "v1:invalid"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestPrepareAdaptiveScheduleLimits(t *testing.T) {
	input := scheduleInput{ID: newWebRunID(), Name: "Адаптивна перевірка", URLs: "https://example.com/", Mode: "list", IntervalHours: 1, MaxIntervalHours: 24, Adaptive: true, NextRunAt: time.Now().Add(time.Hour)}
	source, _, _, err := prepareSchedule(&input, Config{}, 1000)
	if err != nil || !source.Adaptive || source.FailureThreshold != 2 {
		t.Fatalf("unexpected source: %+v %v", source, err)
	}
	input.MaxIntervalHours = 0
	input.FailureThreshold = 11
	if _, _, _, err = prepareSchedule(&input, Config{}, 1000); err == nil {
		t.Fatal("invalid failure threshold accepted")
	}
	input.FailureThreshold = 2
	input.RenderJavaScript = true
	if _, _, _, err = prepareSchedule(&input, Config{}, 1000); err == nil {
		t.Fatal("adaptive JS accepted")
	}
}
