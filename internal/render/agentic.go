package render

import (
	"encoding/json"
	"strings"

	"github.com/chromedp/cdproto/cdp"
)

// Keep only the AX fields used by this audit. Chrome can add enum values to
// unrelated properties (such as ignoredReasons) ahead of the CDP bindings.
type agenticAXTree struct {
	Nodes []*agenticAXNode `json:"nodes"`
}

type agenticAXNode struct {
	Ignored          bool              `json:"ignored"`
	Role             *agenticAXValue   `json:"role"`
	Name             *agenticAXValue   `json:"name"`
	BackendDOMNodeID cdp.BackendNodeID `json:"backendDOMNodeId"`
}

type agenticAXValue struct {
	Value json.RawMessage `json:"value"`
}

// AgenticReport is a deterministic subset of browser accessibility diagnostics,
// not a Lighthouse score or a search-ranking signal.
type AgenticReport struct {
	Status                 string           `json:"status"`
	Explanation            string           `json:"explanation,omitempty"`
	MainLandmarks          int              `json:"main_landmarks"`
	NavigationLandmarks    int              `json:"navigation_landmarks"`
	UnnamedInteractive     int              `json:"unnamed_interactive"`
	UnnamedFormControls    int              `json:"unnamed_form_controls"`
	Findings               []AgenticFinding `json:"findings"`
	FindingsTruncated      bool             `json:"findings_truncated,omitempty"`
	DeclarativeWebMCPForms int              `json:"declarative_webmcp_forms"`
	WebMCPStatus           string           `json:"webmcp_status"`
	WebMCPNote             string           `json:"webmcp_note"`
}

type AgenticFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Role     string `json:"role,omitempty"`
	Element  string `json:"element,omitempty"`
	Message  string `json:"message"`
}

var interactiveAXRoles = map[string]bool{
	"button": true, "link": true, "checkbox": true, "radio": true,
	"combobox": true, "textbox": true, "searchbox": true, "slider": true,
	"spinbutton": true, "switch": true, "tab": true, "menuitem": true,
	"option": true, "listbox": true, "treeitem": true,
}

var formAXRoles = map[string]bool{
	"checkbox": true, "radio": true, "combobox": true, "textbox": true,
	"searchbox": true, "slider": true, "spinbutton": true, "switch": true,
}

func unknownAgenticReport(webmcpForms int) *AgenticReport {
	return &AgenticReport{
		Status:                 "unknown",
		Explanation:            "Chromium не повернув accessibility tree; стан інтерактивних елементів невідомий.",
		Findings:               []AgenticFinding{},
		DeclarativeWebMCPForms: webmcpForms,
		WebMCPStatus:           webMCPStatus(webmcpForms),
		WebMCPNote:             webMCPNote(webmcpForms),
	}
}

func summarizeAXNodes(nodes []*agenticAXNode, truncated bool, webmcpForms int, describe func(cdp.BackendNodeID) string) *AgenticReport {
	if len(nodes) == 0 {
		return unknownAgenticReport(webmcpForms)
	}
	report := &AgenticReport{
		Status:                 "completed",
		Explanation:            "Оцінено доступні вузли accessibility tree основного frame.",
		Findings:               []AgenticFinding{},
		FindingsTruncated:      truncated,
		DeclarativeWebMCPForms: webmcpForms,
		WebMCPStatus:           webMCPStatus(webmcpForms),
		WebMCPNote:             webMCPNote(webmcpForms),
	}
	if truncated {
		report.Status = "partial"
	}
	for _, node := range nodes {
		if node == nil || node.Ignored {
			continue
		}
		role := axString(node.Role)
		switch role {
		case "main":
			report.MainLandmarks++
		case "navigation":
			report.NavigationLandmarks++
		}
		if !interactiveAXRoles[role] || strings.TrimSpace(axString(node.Name)) != "" {
			continue
		}
		report.UnnamedInteractive++
		if formAXRoles[role] {
			report.UnnamedFormControls++
		}
		if len(report.Findings) >= maxAgenticFindings {
			report.FindingsTruncated = true
			continue
		}
		element := "елемент"
		if describe != nil {
			element = describe(node.BackendDOMNodeID)
		}
		report.Findings = append(report.Findings, AgenticFinding{
			Code: "unnamed_interactive", Severity: "warning", Role: role, Element: element,
			Message: "Інтерактивний елемент не має доступного імені; додайте видимий підпис або коректний aria-label.",
		})
	}
	if report.MainLandmarks == 0 && len(report.Findings) < maxAgenticFindings {
		report.Findings = append(report.Findings, AgenticFinding{
			Code: "main_landmark_missing", Severity: "info", Element: "document",
			Message: "Не знайдено main landmark; перевірте, чи основний вміст позначено елементом main або role=main.",
		})
	}
	if report.FindingsTruncated {
		report.Status = "partial"
	}
	return report
}

func axString(value *agenticAXValue) string {
	if value == nil || len(value.Value) == 0 {
		return ""
	}
	var result string
	if json.Unmarshal(value.Value, &result) != nil {
		return ""
	}
	return result
}

func webMCPStatus(forms int) string {
	if forms > 0 {
		return "declarative_forms_observed"
	}
	return "not_observed"
}

func webMCPNote(forms int) string {
	if forms > 0 {
		return "Виявлено декларативні атрибути WebMCP у формах. Реєстрацію інструментів та імперативні API окремо не підтверджено."
	}
	return "Декларативні атрибути WebMCP не виявлені; це інформаційний сигнал, а відсутність не є помилкою SEO."
}
