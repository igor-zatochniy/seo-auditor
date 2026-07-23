package main

import (
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
	data.URL = redactURL(data.URL)
	data.RedirectURL = redactURL(data.RedirectURL)
	data.CanonicalURL = redactURL(data.CanonicalURL)
	data.OGImage = redactURL(data.OGImage)
	data.ErrorMessage = sanitizeErrorMessage(data.ErrorMessage)
	data.URL, data.SafeURLTruncated, data.SafeURLOriginalLength = limitStorageString(data.URL, storageURLMaxRunes)
	data.RedirectURL, data.RedirectURLTruncated, data.RedirectURLOriginalLength = limitStorageString(data.RedirectURL, storageURLMaxRunes)
	data.Title, data.TitleTruncated, data.TitleOriginalLength = limitStorageString(data.Title, storageTitleMaxRunes)
	data.H1, data.H1Truncated, data.H1OriginalLength = limitStorageString(data.H1, storageH1MaxRunes)
	data.OGTitle, data.OGTitleTruncated, data.OGTitleOriginalLength = limitStorageString(data.OGTitle, storageTitleMaxRunes)
	data.OGImage, data.OGImageTruncated, data.OGImageOriginalLength = limitStorageString(data.OGImage, storageURLMaxRunes)
	data.TwitterCard, data.TwitterCardTruncated, data.TwitterCardOriginalLength = limitStorageString(data.TwitterCard, storageTwitterCardMaxRunes)
	data.CanonicalURL, data.CanonicalURLTruncated, data.CanonicalURLOriginalLength = limitStorageString(data.CanonicalURL, storageURLMaxRunes)
	data.MetaRobots, data.MetaRobotsTruncated, data.MetaRobotsOriginalLength = limitStorageString(data.MetaRobots, storageRobotsTagMaxRunes)
	data.XRobotsTag, data.XRobotsTagTruncated, data.XRobotsTagOriginalLength = limitStorageString(data.XRobotsTag, storageRobotsTagMaxRunes)
	return data
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
