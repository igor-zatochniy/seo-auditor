package main

import (
	"io"
	"net/http"

	"github.com/igor-zatochniy/seo-auditor/internal/seo"
)

type SEOData = seo.Data

const (
	storageURLMaxRunes         = seo.StorageURLMaxRunes
	storageTitleMaxRunes       = seo.StorageTitleMaxRunes
	storageDescriptionMaxRunes = seo.StorageDescriptionMaxRunes
	storageH1MaxRunes          = seo.StorageH1MaxRunes
	storageOGTitleMaxRunes     = seo.StorageTitleMaxRunes
	storageTwitterCardMaxRunes = seo.StorageTwitterCardMaxRunes
	storageRobotsTagMaxRunes   = seo.StorageRobotsTagMaxRunes
)

func httpStatus(code int) *int {
	return seo.HTTPStatus(code)
}

func parsePage(resp *http.Response, targetURL string, maxBodyBytes, maxTokenBytes int64) (SEOData, error) {
	return seo.ParsePage(resp, targetURL, maxBodyBytes, maxTokenBytes)
}

func robotsHeaderDirectives(header http.Header) (string, bool, int) {
	return seo.RobotsHeaderDirectives(header)
}

func validateHTMLContentType(raw string) error {
	return seo.ValidateHTMLContentType(raw)
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	return seo.ReadLimited(reader, maxBytes)
}

func isSelfCanonical(canonicalURL, targetURL string) bool {
	return seo.IsSelfCanonical(canonicalURL, targetURL)
}
