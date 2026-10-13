package structureddata

import (
	"encoding/json"
	"strings"
	"testing"
)

func issueCodes(report *Report) map[string]bool {
	result := map[string]bool{}
	for _, issue := range report.Issues {
		result[issue.Code] = true
	}
	return result
}

func TestDiagnosticsSyntaxPropertiesAndEscaping(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"@type":"Product","name":"&amp;quot;Sample&amp;quot;"}`))
	c.AddBlock([]byte(`{"@type":`))
	r := c.Result("https://example.com/")
	codes := issueCodes(r)
	if r.Blocks != 2 || r.ValidBlocks != 1 || !codes["possible_overescaping"] || !codes["product_snippet_properties"] || !codes["invalid_json"] {
		t.Fatalf("unexpected report: %+v", r)
	}
	if r.Issues[len(r.Issues)-1].Offset == 0 {
		t.Fatal("syntax offset missing")
	}
}

func TestDiagnosticsCrossBlockReferencesAndConflicts(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"@type":"Article","author":{"@id":"#person"},"publisher":{"@id":"#missing"}}`))
	c.AddBlock([]byte(`{"@type":"Person","@id":"#person","name":"Author"}`))
	c.AddBlock([]byte(`{"@type":"Organization","@id":"#person","name":"Other"}`))
	r := c.Result("https://example.com/page")
	count := 0
	for _, issue := range r.Issues {
		if issue.Code == "unresolved_local_id" {
			count++
		}
	}
	if count != 1 || !issueCodes(r)["conflicting_id"] {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestDiagnosticsNeverPersistExternalValues(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"@type":"Product","name":"https://example.com/?token=private-fixture","https://private.example/?token=secret":{"@id":"#missing"}}`))
	r := c.Result("https://example.com/")
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-fixture") || strings.Contains(string(encoded), "private.example") {
		t.Fatalf("external data leaked: %s", encoded)
	}
}

func TestDiagnosticsLimitsAndIncompleteReferences(t *testing.T) {
	c := NewCollector()
	c.AddBlock([]byte(`{"author":{"@id":"#missing"}}`))
	c.Incomplete()
	for range 100 {
		c.AddBlock([]byte(`{"@type":"Article"}`))
	}
	r := c.Result("https://example.com/")
	if r.Complete || len(r.Issues) > MaxIssues || issueCodes(r)["unresolved_local_id"] {
		t.Fatalf("invalid bounded report: %+v", r)
	}
}

func TestNoJSONLDIsNotAnError(t *testing.T) {
	r := NewCollector().Result("https://example.com/")
	if r.Blocks != 0 || len(r.Issues) != 0 || !r.Complete {
		t.Fatalf("unexpected absent report: %+v", r)
	}
}
