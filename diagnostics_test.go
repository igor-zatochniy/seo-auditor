package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/igor-zatochniy/seo-auditor/internal/performance"
)

func TestGEODiagnosticsRedactTextWithoutMutatingBlocks(t *testing.T) {
	s := &geo.Signals{Blocks: &geo.BlockSample{Items: []geo.TextBlock{{Text: "https://example.com/?token=private-token", Heading: "https://example.com/?key=private-key", Characters: 350}}}, Performance: &performance.Result{Error: "https://example.com/?secret=private-error"}}
	clean := sanitizeSEODataForStorage(SEOData{GEO: s})
	b, _ := json.Marshal(clean.GEO)
	if strings.Contains(string(b), "private-") {
		t.Fatal("diagnostics leaked secrets")
	}
	if !strings.Contains(s.Blocks.Items[0].Text, "private-token") {
		t.Fatal("mutated caller's signals")
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(`{"blocks":{"items":[{"text":"https://example.com/?token=private-import"}]}}`), &record); err != nil {
		t.Fatal(err)
	}
	sanitizeDiagnosticRecord(record)
	b, _ = json.Marshal(record)
	if strings.Contains(string(b), "private-import") {
		t.Fatal("imported diagnostic URL leaked")
	}
}

func TestRenderingDiagnosticsRedactImportedErrors(t *testing.T) {
	record := map[string]any{"performance": map[string]any{
		"error":    "https://example.com/?token=private-import",
		"warnings": []any{"https://example.com/?key=private-warning"},
	}}
	sanitizeRenderingRecord(record)
	b, err := json.Marshal(record)
	if err != nil || strings.Contains(string(b), "private-") {
		t.Fatalf("imported rendering diagnostics leaked secrets: %v", err)
	}
}
