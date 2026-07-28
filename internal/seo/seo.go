package seo

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
)

const (
	MinTitleLen       = 40
	MaxTitleLen       = 65
	MinDescriptionLen = 120
	MaxDescriptionLen = 170
)

// Data contains the full set of metrics collected by the parser.
type Data struct {
	URL                        string
	SafeURLTruncated           bool
	SafeURLOriginalLength      int
	StatusCode                 *int
	ScanStatus                 string
	ErrorCode                  string
	ErrorMessage               string
	IsRedirect                 bool
	RedirectURL                string
	RedirectURLTruncated       bool
	RedirectURLOriginalLength  int
	Title                      string
	TitleStatus                string
	TitleTruncated             bool
	TitleOriginalLength        int
	Description                string
	DescriptionStatus          string
	H1                         string
	H1Count                    int
	H1Truncated                bool
	H1OriginalLength           int
	H2ToH6Status               string
	OGTitle                    string
	OGTitleTruncated           bool
	OGTitleOriginalLength      int
	OGDescription              string
	OGImage                    string
	OGImageTruncated           bool
	OGImageOriginalLength      int
	TwitterCard                string
	TwitterCardTruncated       bool
	TwitterCardOriginalLength  int
	InternalLinksCount         int
	ExternalLinksCount         int
	LinksCount                 int
	CanonicalURL               string
	CanonicalURLTruncated      bool
	CanonicalURLOriginalLength int
	IsSelfCanonical            bool
	MetaRobots                 string
	MetaRobotsTruncated        bool
	MetaRobotsOriginalLength   int
	XRobotsTag                 string
	XRobotsTagTruncated        bool
	XRobotsTagOriginalLength   int
	RobotsAllowed              bool
	RobotsOutcome              string
	HasJsonLd                  bool
	HasViewport                bool
	TotalImages                int
	ImagesMissingAlt           int
	WordCount                  int
	Duration                   time.Duration
}

func HTTPStatus(code int) *int {
	return &code
}

func ParsePage(resp *http.Response, targetURL string, maxBodyBytes int64) (Data, error) {
	data := Data{
		URL:        targetURL,
		StatusCode: HTTPStatus(resp.StatusCode),
		XRobotsTag: strings.TrimSpace(resp.Header.Get("X-Robots-Tag")),
	}

	if resp.StatusCode != http.StatusOK {
		return data, nil
	}

	body, err := ReadLimited(resp.Body, maxBodyBytes)
	if err != nil {
		return data, err
	}

	decodedBody, err := charset.NewReader(bytes.NewReader(body), resp.Header.Get("Content-Type"))
	if err != nil {
		return data, fmt.Errorf("decode HTML charset: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(decodedBody)
	if err != nil {
		return data, fmt.Errorf("parse HTML: %w", err)
	}

	data.Title = strings.TrimSpace(doc.Find("title").First().Text())
	titleLen := utf8.RuneCountInString(data.Title)
	if titleLen == 0 {
		data.TitleStatus = "Missing"
	} else if titleLen < MinTitleLen {
		data.TitleStatus = "Too Short"
	} else if titleLen > MaxTitleLen {
		data.TitleStatus = "Too Long"
	} else {
		data.TitleStatus = "OK"
	}

	data.Description = strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	descLen := utf8.RuneCountInString(data.Description)
	if descLen == 0 {
		data.DescriptionStatus = "Missing"
	} else if descLen < MinDescriptionLen {
		data.DescriptionStatus = "Too Short"
	} else if descLen > MaxDescriptionLen {
		data.DescriptionStatus = "Too Long"
	} else {
		data.DescriptionStatus = "OK"
	}

	h1Selection := doc.Find("h1")
	data.H1Count = h1Selection.Length()
	if data.H1Count > 0 {
		data.H1 = strings.TrimSpace(h1Selection.First().Text())
	} else {
		data.H1 = "[Missing H1]"
	}

	var subHeaders []string
	for i := 2; i <= 6; i++ {
		count := doc.Find(fmt.Sprintf("h%d", i)).Length()
		if count > 0 {
			subHeaders = append(subHeaders, fmt.Sprintf("H%d:%d", i, count))
		}
	}
	if len(subHeaders) > 0 {
		data.H2ToH6Status = strings.Join(subHeaders, ", ")
	} else {
		data.H2ToH6Status = "No sub-headers (H2-H6)"
	}

	data.OGTitle = strings.TrimSpace(doc.Find("meta[property='og:title']").AttrOr("content", ""))
	data.OGDescription = strings.TrimSpace(doc.Find("meta[property='og:description']").AttrOr("content", ""))
	data.OGImage = strings.TrimSpace(doc.Find("meta[property='og:image']").AttrOr("content", ""))
	data.TwitterCard = strings.TrimSpace(doc.Find("meta[name='twitter:card']").AttrOr("content", ""))
	baseParsed, _ := url.Parse(targetURL)
	doc.Find("a").Each(func(_ int, selection *goquery.Selection) {
		href, exists := selection.Attr("href")
		if !exists {
			return
		}
		href = strings.TrimSpace(href)
		hrefLower := strings.ToLower(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(hrefLower, "javascript:") || strings.HasPrefix(hrefLower, "mailto:") || strings.HasPrefix(hrefLower, "tel:") {
			return
		}

		linkParsed, err := url.Parse(href)
		if err != nil {
			return
		}

		if linkParsed.Host == "" || strings.EqualFold(linkParsed.Host, baseParsed.Host) {
			data.InternalLinksCount++
			return
		}
		data.ExternalLinksCount++
	})
	data.LinksCount = data.InternalLinksCount + data.ExternalLinksCount

	data.CanonicalURL = strings.TrimSpace(doc.Find("link[rel='canonical']").AttrOr("href", ""))
	data.IsSelfCanonical = IsSelfCanonical(data.CanonicalURL, targetURL)
	data.MetaRobots = strings.TrimSpace(doc.Find("meta[name='robots']").AttrOr("content", ""))

	data.HasJsonLd = doc.Find("script[type='application/ld+json']").Length() > 0
	data.HasViewport = doc.Find("meta[name='viewport']").Length() > 0

	imgSelection := doc.Find("img")
	data.TotalImages = imgSelection.Length()
	imgSelection.Each(func(_ int, selection *goquery.Selection) {
		alt, exists := selection.Attr("alt")
		if !exists || strings.TrimSpace(alt) == "" {
			data.ImagesMissingAlt++
		}
	})

	bodyText := doc.Find("body").Text()
	words := strings.Fields(bodyText)
	data.WordCount = len(words)

	return data, nil
}

func ValidateHTMLContentType(raw string) error {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("parse Content-Type: %w", err)
	}
	switch strings.ToLower(mediaType) {
	case "text/html", "application/xhtml+xml":
		return nil
	default:
		return fmt.Errorf("media type %q is not HTML", mediaType)
	}
}

func ReadLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive")
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds configured limit of %d bytes", maxBytes)
	}
	return body, nil
}

func IsSelfCanonical(canonicalURL, targetURL string) bool {
	if strings.TrimSpace(canonicalURL) == "" {
		return false
	}

	targetParsed, err := url.Parse(targetURL)
	if err != nil {
		return false
	}

	canonicalParsed, err := url.Parse(canonicalURL)
	if err != nil {
		return false
	}
	if !canonicalParsed.IsAbs() {
		canonicalParsed = targetParsed.ResolveReference(canonicalParsed)
	}

	normalize := func(parsed *url.URL) string {
		copyValue := *parsed
		copyValue.Scheme = strings.ToLower(copyValue.Scheme)
		copyValue.Host = strings.ToLower(copyValue.Host)
		copyValue.Fragment = ""
		return strings.TrimRight(copyValue.String(), "/")
	}

	return normalize(canonicalParsed) == normalize(targetParsed)
}
