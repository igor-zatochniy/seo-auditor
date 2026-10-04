package geo

// Signals містить лише обмежену вибірку контенту, а не HTML або виконаний JavaScript.
type Signals struct {
	Version          int      `json:"version"`
	Complete         bool     `json:"complete"`
	Excerpt          string   `json:"excerpt"`
	Headings         string   `json:"headings"`
	FirstParagraph   string   `json:"first_paragraph"`
	SampleTruncated  bool     `json:"sample_truncated"`
	HasTable         bool     `json:"has_table"`
	HasList          bool     `json:"has_list"`
	HasAuthor        bool     `json:"has_author"`
	SchemaTypes      []string `json:"schema_types"`
	SchemaIncomplete bool     `json:"schema_incomplete"`
}
