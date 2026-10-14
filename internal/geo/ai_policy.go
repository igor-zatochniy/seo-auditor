package geo

import (
	"errors"
	"strings"
	"time"
)

// AIPolicySpec describes an official robots.txt product token. It is not a
// claim that the provider has fetched, indexed, or cited a page.
type AIPolicySpec struct {
	Token            string `json:"token"`
	Provider         string `json:"provider"`
	Purpose          string `json:"purpose"`
	PurposeLabel     string `json:"purpose_label"`
	HasHTTPUserAgent bool   `json:"has_http_user_agent"`
	DocumentationURL string `json:"documentation_url"`
}

// AIControl is the per-URL evidence observed in the site's robots.txt.
type AIControl struct {
	Token             string     `json:"token"`
	Provider          string     `json:"provider"`
	Purpose           string     `json:"purpose"`
	Status            string     `json:"status"`
	Directive         string     `json:"directive,omitempty"`
	ConflictDirective string     `json:"conflict_directive,omitempty"`
	RobotsURL         string     `json:"robots_url,omitempty"`
	CheckedAt         *time.Time `json:"checked_at,omitempty"`
	HTTPStatus        *int       `json:"robots_http_status,omitempty"`
	Confidence        string     `json:"confidence"`
	Conflict          bool       `json:"conflict,omitempty"`
	Explanation       string     `json:"explanation,omitempty"`
}

var SupportedAIPolicySpecs = []AIPolicySpec{
	{Token: "Googlebot", Provider: "Google", Purpose: "search", PurposeLabel: "Google Search та AI-функції пошуку", HasHTTPUserAgent: true, DocumentationURL: "https://developers.google.com/crawling/docs/crawlers-fetchers/google-common-crawlers"},
	{Token: "Google-Extended", Provider: "Google", Purpose: "training_grounding", PurposeLabel: "Навчання моделей та обґрунтування відповідей в окремих Google-продуктах", HasHTTPUserAgent: false, DocumentationURL: "https://developers.google.com/crawling/docs/crawlers-fetchers/google-common-crawlers"},
	{Token: "OAI-SearchBot", Provider: "OpenAI", Purpose: "search", PurposeLabel: "ChatGPT Search", HasHTTPUserAgent: true, DocumentationURL: "https://developers.openai.com/api/docs/bots"},
	{Token: "GPTBot", Provider: "OpenAI", Purpose: "training", PurposeLabel: "Можливе використання для навчання моделей OpenAI", HasHTTPUserAgent: true, DocumentationURL: "https://developers.openai.com/api/docs/bots"},
	{Token: "Claude-SearchBot", Provider: "Anthropic", Purpose: "search", PurposeLabel: "Пошук у Claude", HasHTTPUserAgent: true, DocumentationURL: "https://support.anthropic.com/en/articles/8896518-does-anthropic-crawl-data-from-the-web-and-how-can-site-owners-block-the-crawler"},
	{Token: "ClaudeBot", Provider: "Anthropic", Purpose: "training", PurposeLabel: "Збір контенту для покращення моделей Anthropic", HasHTTPUserAgent: true, DocumentationURL: "https://support.anthropic.com/en/articles/8896518-does-anthropic-crawl-data-from-the-web-and-how-can-site-owners-block-the-crawler"},
	{Token: "PerplexityBot", Provider: "Perplexity", Purpose: "search", PurposeLabel: "Індексування для пошукових результатів Perplexity", HasHTTPUserAgent: true, DocumentationURL: "https://docs.perplexity.ai/docs/resources/perplexity-crawlers"},
	{Token: "Applebot", Provider: "Apple", Purpose: "search", PurposeLabel: "Spotlight, Siri та пошук Apple", HasHTTPUserAgent: true, DocumentationURL: "https://support.apple.com/en-us/119829"},
	{Token: "Applebot-Extended", Provider: "Apple", Purpose: "training", PurposeLabel: "Використання контенту для навчання базових моделей Apple", HasHTTPUserAgent: false, DocumentationURL: "https://support.apple.com/en-us/119829"},
}

func SupportedAIPolicyTokens() []string {
	tokens := make([]string, len(SupportedAIPolicySpecs))
	for i, spec := range SupportedAIPolicySpecs {
		tokens[i] = strings.ToLower(spec.Token)
	}
	return tokens
}

// NormalizeDesiredAIPolicy fills unspecified preferences with unknown and
// rejects arbitrary tokens or values before they are stored.
func NormalizeDesiredAIPolicy(input map[string]string) (map[string]string, error) {
	known := make(map[string]struct{}, len(SupportedAIPolicySpecs))
	out := make(map[string]string, len(SupportedAIPolicySpecs))
	for _, spec := range SupportedAIPolicySpecs {
		key := strings.ToLower(spec.Token)
		known[key] = struct{}{}
		out[key] = Unknown
	}
	for raw, value := range input {
		key := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := known[key]; !ok {
			return nil, errors.New("бажана AI-політика містить непідтримуваний токен")
		}
		if value != Allowed && value != Blocked && value != Unknown {
			return nil, errors.New("бажаний стан AI-політики має бути allowed, blocked або unknown")
		}
		out[key] = value
	}
	return out, nil
}
