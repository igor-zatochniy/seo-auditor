package render

import (
	"encoding/json"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	jsonv2 "github.com/go-json-experiment/json"
)

func testAXValue(t *testing.T, raw string) *agenticAXValue {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &agenticAXValue{Value: encoded}
}

func testAXNode(t *testing.T, role, name string, ignored bool) *agenticAXNode {
	return &agenticAXNode{Role: testAXValue(t, role), Name: testAXValue(t, name), Ignored: ignored, BackendDOMNodeID: cdp.BackendNodeID(7)}
}

func TestSummarizeAXNodesCountsLandmarksAndUnnamedControls(t *testing.T) {
	nodes := []*agenticAXNode{
		testAXNode(t, "main", "", false),
		testAXNode(t, "navigation", "Primary", false),
		testAXNode(t, "button", "Save", false),
		testAXNode(t, "button", "", false),
		testAXNode(t, "textbox", "", false),
		testAXNode(t, "link", "", true),
	}
	report := summarizeAXNodes(nodes, false, 2, func(cdp.BackendNodeID) string { return "button#save" })
	if report.Status != "completed" || report.MainLandmarks != 1 || report.NavigationLandmarks != 1 || report.UnnamedInteractive != 2 || report.UnnamedFormControls != 1 {
		t.Fatalf("unexpected accessibility summary: %+v", report)
	}
	if len(report.Findings) != 2 || report.Findings[0].Element != "button#save" || report.WebMCPStatus != "declarative_forms_observed" {
		t.Fatalf("unexpected findings/WebMCP summary: %+v", report)
	}
}

func TestSummarizeAXNodesMarksMissingMainAndTruncation(t *testing.T) {
	report := summarizeAXNodes([]*agenticAXNode{testAXNode(t, "button", "", false)}, true, 0, nil)
	if report.Status != "partial" || !report.FindingsTruncated || report.WebMCPStatus != "not_observed" {
		t.Fatalf("expected partial, truncated report: %+v", report)
	}
	if report.Findings[0].Code != "unnamed_interactive" {
		t.Fatalf("missing interactive finding: %+v", report.Findings)
	}
	report = summarizeAXNodes(nil, false, 0, nil)
	if report.Status != "unknown" || len(report.Findings) != 0 {
		t.Fatalf("empty accessibility tree should be unknown: %+v", report)
	}
}

func TestSummarizeAXNodesCapsFindingsButKeepsCounts(t *testing.T) {
	nodes := make([]*agenticAXNode, maxAgenticFindings+9)
	for i := range nodes {
		nodes[i] = testAXNode(t, "button", "", false)
	}
	report := summarizeAXNodes(nodes, false, 0, nil)
	if report.Status != "partial" || !report.FindingsTruncated || len(report.Findings) != maxAgenticFindings || report.UnnamedInteractive != len(nodes) {
		t.Fatalf("findings cap did not preserve full counts: %+v", report)
	}
}

func TestUnknownAgenticReportDoesNotInventFailures(t *testing.T) {
	report := unknownAgenticReport(0)
	if report.Status != "unknown" || report.MainLandmarks != 0 || report.UnnamedInteractive != 0 || len(report.Findings) != 0 {
		t.Fatalf("unknown rendering should not become a failed audit: %+v", report)
	}
}

func TestAgenticAXTreeToleratesNewChromePropertyEnums(t *testing.T) {
	var tree agenticAXTree
	err := jsonv2.Unmarshal([]byte(`{"nodes":[
		{"ignored":true,"ignoredReasons":[{"name":"uninteresting","value":{"type":"boolean","value":true}}]},
		{"ignored":false,"role":{"type":"role","value":"main"},"name":{"type":"computedString","value":""}},
		{"ignored":false,"role":{"type":"role","value":"button"},"name":{"type":"computedString","value":""},"backendDOMNodeId":7,"properties":[{"name":"futureProperty","value":{"type":"futureType","value":true}}]}
	]}`), &tree)
	if err != nil {
		t.Fatalf("unused CDP fields broke AX decoding: %v", err)
	}
	report := summarizeAXNodes(tree.Nodes, false, 0, nil)
	if report.Status != "completed" || report.MainLandmarks != 1 || report.UnnamedInteractive != 1 {
		t.Fatalf("AX projection lost supported audit fields: %+v", report)
	}
}
