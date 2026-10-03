package seo

// GooglebotHTMLRiskBytes is a body-only risk threshold, not an indexing guarantee.
// HTTP headers also consume Google's budget; the parser safety limit is separate.
const GooglebotHTMLRiskBytes int64 = 2 * 1024 * 1024

func googlebotSizeStatus(bytes int64, complete bool) string {
	if bytes >= GooglebotHTMLRiskBytes {
		return "Googlebot cutoff risk"
	}
	if complete {
		return "OK"
	}
	return "Unknown (incomplete HTML)"
}
