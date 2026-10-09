package geo

import "github.com/igor-zatochniy/seo-auditor/internal/performance"

// Signals містить лише обмежену вибірку контенту, а не HTML або виконаний JavaScript.
type Signals struct {
	Version           int                 `json:"version"`
	Complete          bool                `json:"complete"`
	Excerpt           string              `json:"excerpt"`
	Headings          string              `json:"headings"`
	FirstParagraph    string              `json:"first_paragraph"`
	SampleTruncated   bool                `json:"sample_truncated"`
	HasTable          bool                `json:"has_table"`
	HasList           bool                `json:"has_list"`
	HasAuthor         bool                `json:"has_author"`
	SchemaTypes       []string            `json:"schema_types"`
	SchemaIncomplete  bool                `json:"schema_incomplete"`
	ContentSource     string              `json:"content_source,omitempty"`
	ContentIncomplete bool                `json:"content_incomplete,omitempty"`
	Search            SearchControls      `json:"search"`
	Blocks            *BlockSample        `json:"blocks,omitempty"`
	HTTP              *HTTPObservation    `json:"http,omitempty"`
	Performance       *performance.Result `json:"performance,omitempty"`
	Eligibility       *AIEligibility      `json:"eligibility,omitempty"`
	Entities          *EntitySample       `json:"entities,omitempty"`
}

const (
	Allowed = "allowed"
	Blocked = "blocked"
	Unknown = "unknown"
)

// SearchControls описує лише прочитані правила, а не індекс чи фактичне цитування.
type SearchControls struct {
	GooglebotRules    string `json:"googlebot_rules"`
	OAISearchBotRules string `json:"oai_searchbot_rules"`
	GPTBotRules       string `json:"gptbot_rules,omitempty"`
	GoogleIndexing    string `json:"google_indexing"`
	GoogleSnippet     string `json:"google_snippet"`
	MaxSnippet        *int   `json:"max_snippet,omitempty"`
	DataNoSnippet     bool   `json:"data_nosnippet"`
}

// HTTPObservation описує вимір нашого клієнта, а не доступ із мережі AI-провайдера.
type HTTPObservation struct {
	Status     int    `json:"status"`
	ResponseMS *int64 `json:"response_ms"`
	Slow       bool   `json:"slow"`
}

type TextBlock struct {
	Heading    string `json:"heading"`
	Text       string `json:"text"`
	Characters int    `json:"characters"`
}

type BlockSample struct {
	Count    int         `json:"count"`
	Complete bool        `json:"complete"`
	Items    []TextBlock `json:"items"`
}
