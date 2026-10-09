package main

import (
	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxStoredErrorLength = 1000

var sensitiveQueryKeys = map[string]struct{}{
	"token":             {},
	"accesstoken":       {},
	"apikey":            {},
	"key":               {},
	"secret":            {},
	"clientsecret":      {},
	"privatekey":        {},
	"password":          {},
	"signature":         {},
	"sig":               {},
	"xamzsignature":     {},
	"xgoogsignature":    {},
	"code":              {},
	"authorization":     {},
	"auth":              {},
	"authtoken":         {},
	"jwt":               {},
	"idtoken":           {},
	"refreshtoken":      {},
	"session":           {},
	"sessionid":         {},
	"xamzcredential":    {},
	"xamzsecuritytoken": {},
}

var sensitiveQueryKeyFragments = []string{
	"token",
	"secret",
	"signature",
	"credential",
	"password",
	"passwd",
	"privatekey",
	"apikey",
	"accesskey",
	"session",
	"authorization",
	"jwt",
}

var urlInTextPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func redactURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "[invalid-url]"
	}

	query := parsed.Query()
	for key := range query {
		if isSensitiveQueryKey(key) {
			query.Set(key, "[REDACTED]")
			continue
		}
		query.Set(key, "[VALUE]")
	}

	parsed.RawQuery = query.Encode()
	parsed.User = nil
	parsed.Fragment = ""

	return parsed.String()
}

func isSensitiveQueryKey(key string) bool {
	normalized := normalizeQueryKey(key)
	if _, sensitive := sensitiveQueryKeys[normalized]; sensitive {
		return true
	}
	for _, fragment := range sensitiveQueryKeyFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func normalizeQueryKey(key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	var builder strings.Builder
	builder.Grow(len(lower))
	for _, r := range lower {
		switch r {
		case '_', '-', '.', ' ':
			continue
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func redactText(raw string) string {
	if raw == "" {
		return ""
	}

	return urlInTextPattern.ReplaceAllStringFunc(raw, func(candidate string) string {
		urlValue, suffix := trimTrailingURLPunctuation(candidate)
		return redactURL(urlValue) + suffix
	})
}

func sanitizeErrorMessage(message string) string {
	return truncateStoredErrorMessage(redactText(message))
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrorMessage(err.Error())
}

func sanitizeSEODataForStorage(data SEOData) SEOData {
	if data.Rendering != nil {
		data.Rendering.Sanitize(func(v string) string { return sanitizeGEOTextForStorage(v, 8192) }, redactURL)
	}
	if data.GEO != nil {
		signals := *data.GEO
		signals.Excerpt = sanitizeGEOTextForStorage(signals.Excerpt, 2000)
		signals.Headings = sanitizeGEOTextForStorage(signals.Headings, 800)
		signals.FirstParagraph = sanitizeGEOTextForStorage(signals.FirstParagraph, 600)
		if signals.Entities != nil {
			copy := *signals.Entities
			clean := func(records []geo.EntityRecord) []geo.EntityRecord {
				result := append([]geo.EntityRecord(nil), records...)
				for i := range result {
					result[i].Name = sanitizeGEOTextForStorage(result[i].Name, 200)
					result[i].Types = append([]string(nil), result[i].Types...)
					for j := range result[i].Types {
						result[i].Types[j] = sanitizeGEOTextForStorage(result[i].Types[j], 100)
					}
					result[i].SameAs = append([]string(nil), result[i].SameAs...)
					for j := range result[i].SameAs {
						result[i].SameAs[j] = redactURL(result[i].SameAs[j])
					}
				}
				return result
			}
			copy.Organizations, copy.Authors = clean(copy.Organizations), clean(copy.Authors)
			copy.Publishers, copy.ProductBrands = clean(copy.Publishers), clean(copy.ProductBrands)
			signals.Entities = &copy
		}
		if signals.Blocks != nil {
			copy := *signals.Blocks
			copy.Items = append([]geo.TextBlock(nil), copy.Items...)
			for i := range copy.Items {
				copy.Items[i].Text = sanitizeGEOTextForStorage(copy.Items[i].Text, 500)
				copy.Items[i].Heading = sanitizeGEOTextForStorage(copy.Items[i].Heading, 200)
			}
			signals.Blocks = &copy
		}
		if signals.Performance != nil {
			copy := *signals.Performance
			copy.Error = sanitizeGEOTextForStorage(copy.Error, 1000)
			signals.Performance = &copy
		}
		data.GEO = &signals
		data.GEO.Eligibility = geo.EvaluateEligibility(data.GEO)
	}
	data.URL = redactURL(data.URL)
	data.RedirectURL = redactURL(data.RedirectURL)
	data.CanonicalURL = redactURL(data.CanonicalURL)
	data.OGImage = redactURL(data.OGImage)
	data.ErrorMessage = sanitizeErrorMessage(data.ErrorMessage)
	data.URL, data.SafeURLTruncated, data.SafeURLOriginalLength = limitStorageString(data.URL, storageURLMaxRunes)
	data.RedirectURL, data.RedirectURLTruncated, data.RedirectURLOriginalLength = limitStorageString(data.RedirectURL, storageURLMaxRunes)
	data.Title, data.TitleTruncated, data.TitleOriginalLength = limitStorageStringPreservingMetadata(
		data.Title, storageTitleMaxRunes, data.TitleTruncated, data.TitleOriginalLength,
	)
	data.Description, _, _ = limitStorageString(data.Description, storageDescriptionMaxRunes)
	data.H1, data.H1Truncated, data.H1OriginalLength = limitStorageStringPreservingMetadata(
		data.H1, storageH1MaxRunes, data.H1Truncated, data.H1OriginalLength,
	)
	data.OGTitle, data.OGTitleTruncated, data.OGTitleOriginalLength = limitStorageStringPreservingMetadata(
		data.OGTitle, storageTitleMaxRunes, data.OGTitleTruncated, data.OGTitleOriginalLength,
	)
	data.OGDescription, _, _ = limitStorageString(data.OGDescription, storageDescriptionMaxRunes)
	data.OGImage, data.OGImageTruncated, data.OGImageOriginalLength = limitStorageStringPreservingMetadata(
		data.OGImage, storageURLMaxRunes, data.OGImageTruncated, data.OGImageOriginalLength,
	)
	data.TwitterCard, data.TwitterCardTruncated, data.TwitterCardOriginalLength = limitStorageStringPreservingMetadata(
		data.TwitterCard, storageTwitterCardMaxRunes, data.TwitterCardTruncated, data.TwitterCardOriginalLength,
	)
	data.CanonicalURL, data.CanonicalURLTruncated, data.CanonicalURLOriginalLength = limitStorageStringPreservingMetadata(
		data.CanonicalURL, storageURLMaxRunes, data.CanonicalURLTruncated, data.CanonicalURLOriginalLength,
	)
	data.MetaRobots, data.MetaRobotsTruncated, data.MetaRobotsOriginalLength = limitStorageStringPreservingMetadata(
		data.MetaRobots, storageRobotsTagMaxRunes, data.MetaRobotsTruncated, data.MetaRobotsOriginalLength,
	)
	data.XRobotsTag, data.XRobotsTagTruncated, data.XRobotsTagOriginalLength = limitStorageStringPreservingMetadata(
		data.XRobotsTag, storageRobotsTagMaxRunes, data.XRobotsTagTruncated, data.XRobotsTagOriginalLength,
	)
	return data
}

func sanitizeGEOTextForStorage(value string, maxRunes int) string {
	// PostgreSQL JSONB не приймає U+0000 навіть у JSON escape-послідовності.
	value = strings.ReplaceAll(value, "\x00", "\uFFFD")
	return truncateRunes(redactText(value), maxRunes)
}

func limitStorageStringPreservingMetadata(
	value string,
	maxRunes int,
	alreadyTruncated bool,
	originalLength int,
) (string, bool, int) {
	limitedValue, truncatedNow, measuredLength := limitStorageString(value, maxRunes)
	if !alreadyTruncated {
		return limitedValue, truncatedNow, measuredLength
	}
	if originalLength < measuredLength {
		originalLength = measuredLength
	}
	return limitedValue, true, originalLength
}

func limitStorageString(value string, maxRunes int) (string, bool, int) {
	originalLength := utf8.RuneCountInString(value)
	if originalLength <= maxRunes {
		return value, false, 0
	}
	return truncateRunes(value, maxRunes), true, originalLength
}

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runeCount := 0
	for index := range value {
		if runeCount == maxRunes {
			return value[:index]
		}
		runeCount++
	}
	return value
}

func truncateStoredErrorMessage(message string) string {
	if len(message) <= maxStoredErrorLength {
		return message
	}

	end := 0
	for end < len(message) {
		_, size := utf8.DecodeRuneInString(message[end:])
		if end+size > maxStoredErrorLength {
			break
		}
		end += size
	}
	return message[:end] + "..."
}

func trimTrailingURLPunctuation(raw string) (string, string) {
	urlValue := raw
	suffix := ""
	for len(urlValue) > 0 {
		last := urlValue[len(urlValue)-1]
		if !strings.ContainsRune(".,;:)]}", rune(last)) {
			break
		}
		urlValue = urlValue[:len(urlValue)-1]
		suffix = string(last) + suffix
	}
	return urlValue, suffix
}
