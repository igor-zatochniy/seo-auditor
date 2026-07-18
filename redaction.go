package main

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxStoredErrorLength = 1000

var sensitiveQueryKeys = map[string]struct{}{
	"token":         {},
	"access_token":  {},
	"access-token":  {},
	"api_key":       {},
	"api-key":       {},
	"key":           {},
	"secret":        {},
	"client_secret": {},
	"client-secret": {},
	"password":      {},
	"signature":     {},
	"sig":           {},
	"code":          {},
	"authorization": {},
	"auth":          {},
	"auth_token":    {},
	"auth-token":    {},
	"jwt":           {},
	"id_token":      {},
	"id-token":      {},
	"refresh_token": {},
	"refresh-token": {},
	"session":       {},
	"session_id":    {},
	"session-id":    {},
	"sessionid":     {},
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
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(key)]; sensitive {
			query.Set(key, "[REDACTED]")
		}
	}

	parsed.RawQuery = query.Encode()
	parsed.User = nil
	parsed.Fragment = ""

	return parsed.String()
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
	return data
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
