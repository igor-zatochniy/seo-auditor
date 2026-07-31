package seo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	MinTitleLen       = 40
	MaxTitleLen       = 65
	MinDescriptionLen = 120
	MaxDescriptionLen = 170

	StorageURLMaxRunes         = 2048
	StorageTitleMaxRunes       = 500
	StorageH1MaxRunes          = 1000
	StorageTwitterCardMaxRunes = 100
	StorageRobotsTagMaxRunes   = 200
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

func ParsePage(resp *http.Response, targetURL string, maxBodyBytes, maxTokenBytes int64) (Data, error) {
	data := Data{
		URL:        targetURL,
		StatusCode: HTTPStatus(resp.StatusCode),
	}
	data.XRobotsTag, data.XRobotsTagTruncated, data.XRobotsTagOriginalLength =
		boundedString(strings.TrimSpace(resp.Header.Get("X-Robots-Tag")), StorageRobotsTagMaxRunes)

	if resp.StatusCode != http.StatusOK {
		return data, nil
	}
	if maxBodyBytes <= 0 {
		return data, fmt.Errorf("maxBodyBytes must be positive")
	}
	if maxTokenBytes <= 0 {
		return data, fmt.Errorf("maxTokenBytes must be positive")
	}

	rawBody := &countingReader{
		reader: io.LimitReader(resp.Body, maxBodyBytes+1),
	}
	decodedBody, err := charset.NewReader(rawBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return data, fmt.Errorf("decode HTML charset: %w", err)
	}

	tokenizer := html.NewTokenizer(decodedBody)
	tokenLimit := min(maxTokenBytes, maxBodyBytes+1)
	tokenizer.SetMaxBuf(int(tokenLimit))
	parser := newPageParser(&data, targetURL)

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			if rawBody.bytesRead > maxBodyBytes {
				return data, fmt.Errorf("response body exceeds configured limit of %d bytes", maxBodyBytes)
			}
			tokenErr := tokenizer.Err()
			if errors.Is(tokenErr, io.EOF) {
				parser.finalize()
				return data, nil
			}
			if errors.Is(tokenErr, html.ErrBufferExceeded) {
				return data, fmt.Errorf("HTML token exceeds configured limit of %d bytes", tokenLimit)
			}
			return data, fmt.Errorf("tokenize HTML: %w", tokenErr)
		case html.TextToken:
			parser.handleText(tokenizer.Text())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttributes := tokenizer.TagName()
			attributes := readTagAttributes(tokenizer, hasAttributes)
			parser.handleStartTag(name, attributes)
			if tokenType == html.SelfClosingTagToken {
				parser.handleEndTag(name)
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			parser.handleEndTag(name)
		}
	}
}

type countingReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.bytesRead += int64(count)
	return count, err
}

type tagAttributes struct {
	name        []byte
	property    []byte
	content     []byte
	rel         []byte
	href        []byte
	typeValue   []byte
	alt         []byte
	hasName     bool
	hasProperty bool
	hasContent  bool
	hasRel      bool
	hasHref     bool
	hasType     bool
	hasAlt      bool
}

func readTagAttributes(tokenizer *html.Tokenizer, hasAttributes bool) tagAttributes {
	var attributes tagAttributes
	for hasAttributes {
		key, value, moreAttributes := tokenizer.TagAttr()
		switch {
		case bytes.Equal(key, []byte("name")):
			attributes.name = value
			attributes.hasName = true
		case bytes.Equal(key, []byte("property")):
			attributes.property = value
			attributes.hasProperty = true
		case bytes.Equal(key, []byte("content")):
			attributes.content = value
			attributes.hasContent = true
		case bytes.Equal(key, []byte("rel")):
			attributes.rel = value
			attributes.hasRel = true
		case bytes.Equal(key, []byte("href")):
			attributes.href = value
			attributes.hasHref = true
		case bytes.Equal(key, []byte("type")):
			attributes.typeValue = value
			attributes.hasType = true
		case bytes.Equal(key, []byte("alt")):
			attributes.alt = value
			attributes.hasAlt = true
		}
		hasAttributes = moreAttributes
	}
	return attributes
}

type pageParser struct {
	data              *Data
	baseURL           *url.URL
	title             boundedTextCollector
	firstH1           boundedTextCollector
	titleSeen         bool
	collectTitle      bool
	collectFirstH1    bool
	descriptionSeen   bool
	ogTitleSeen       bool
	ogDescriptionSeen bool
	ogImageSeen       bool
	twitterCardSeen   bool
	canonicalSeen     bool
	metaRobotsSeen    bool
	headDepth         int
	subHeaderCounts   [7]int
	bodyWordCounter   wordCounter
}

func newPageParser(data *Data, targetURL string) *pageParser {
	baseURL, _ := url.Parse(targetURL)
	return &pageParser{
		data:    data,
		baseURL: baseURL,
		title:   newBoundedTextCollector(StorageTitleMaxRunes),
		firstH1: newBoundedTextCollector(StorageH1MaxRunes),
	}
}

func (p *pageParser) handleStartTag(name []byte, attributes tagAttributes) {
	switch {
	case bytes.Equal(name, []byte("title")):
		if !p.titleSeen {
			p.titleSeen = true
			p.collectTitle = true
		}
	case bytes.Equal(name, []byte("head")):
		p.headDepth++
	case bytes.Equal(name, []byte("h1")):
		p.data.H1Count++
		if p.data.H1Count == 1 {
			p.collectFirstH1 = true
		}
	case bytes.Equal(name, []byte("h2")):
		p.subHeaderCounts[2]++
	case bytes.Equal(name, []byte("h3")):
		p.subHeaderCounts[3]++
	case bytes.Equal(name, []byte("h4")):
		p.subHeaderCounts[4]++
	case bytes.Equal(name, []byte("h5")):
		p.subHeaderCounts[5]++
	case bytes.Equal(name, []byte("h6")):
		p.subHeaderCounts[6]++
	case bytes.Equal(name, []byte("meta")):
		p.handleMeta(attributes)
	case bytes.Equal(name, []byte("link")):
		if !p.canonicalSeen && attributes.hasRel && attributes.hasHref &&
			bytes.EqualFold(bytes.TrimSpace(attributes.rel), []byte("canonical")) {
			p.canonicalSeen = true
			p.data.CanonicalURL, p.data.CanonicalURLTruncated, p.data.CanonicalURLOriginalLength =
				boundedBytes(attributes.href, StorageURLMaxRunes)
		}
	case bytes.Equal(name, []byte("script")):
		if attributes.hasType &&
			bytes.EqualFold(bytes.TrimSpace(attributes.typeValue), []byte("application/ld+json")) {
			p.data.HasJsonLd = true
		}
	case bytes.Equal(name, []byte("a")):
		if attributes.hasHref {
			p.countLink(attributes.href)
		}
	case bytes.Equal(name, []byte("img")):
		p.data.TotalImages++
		if !attributes.hasAlt || len(bytes.TrimSpace(attributes.alt)) == 0 {
			p.data.ImagesMissingAlt++
		}
	}
}

func (p *pageParser) handleMeta(attributes tagAttributes) {
	if attributes.hasName {
		name := bytes.TrimSpace(attributes.name)
		switch {
		case bytes.EqualFold(name, []byte("description")) && !p.descriptionSeen:
			p.descriptionSeen = true
			if attributes.hasContent {
				p.data.Description = strings.TrimSpace(string(attributes.content))
			}
		case bytes.EqualFold(name, []byte("twitter:card")) && !p.twitterCardSeen:
			p.twitterCardSeen = true
			if attributes.hasContent {
				p.data.TwitterCard, p.data.TwitterCardTruncated, p.data.TwitterCardOriginalLength =
					boundedBytes(attributes.content, StorageTwitterCardMaxRunes)
			}
		case bytes.EqualFold(name, []byte("robots")) && !p.metaRobotsSeen:
			p.metaRobotsSeen = true
			if attributes.hasContent {
				p.data.MetaRobots, p.data.MetaRobotsTruncated, p.data.MetaRobotsOriginalLength =
					boundedBytes(attributes.content, StorageRobotsTagMaxRunes)
			}
		case bytes.EqualFold(name, []byte("viewport")):
			p.data.HasViewport = true
		}
	}

	if !attributes.hasProperty || !attributes.hasContent {
		return
	}
	property := bytes.TrimSpace(attributes.property)
	switch {
	case bytes.EqualFold(property, []byte("og:title")) && !p.ogTitleSeen:
		p.ogTitleSeen = true
		p.data.OGTitle, p.data.OGTitleTruncated, p.data.OGTitleOriginalLength =
			boundedBytes(attributes.content, StorageTitleMaxRunes)
	case bytes.EqualFold(property, []byte("og:description")) && !p.ogDescriptionSeen:
		p.ogDescriptionSeen = true
		p.data.OGDescription = strings.TrimSpace(string(attributes.content))
	case bytes.EqualFold(property, []byte("og:image")) && !p.ogImageSeen:
		p.ogImageSeen = true
		p.data.OGImage, p.data.OGImageTruncated, p.data.OGImageOriginalLength =
			boundedBytes(attributes.content, StorageURLMaxRunes)
	}
}

func (p *pageParser) countLink(rawHref []byte) {
	href := strings.TrimSpace(string(rawHref))
	if href == "" || strings.HasPrefix(href, "#") ||
		hasASCIIPrefixFold(href, "javascript:") ||
		hasASCIIPrefixFold(href, "mailto:") ||
		hasASCIIPrefixFold(href, "tel:") {
		return
	}

	parsedLink, err := url.Parse(href)
	if err != nil {
		return
	}
	if parsedLink.Host == "" || p.baseURL != nil && strings.EqualFold(parsedLink.Host, p.baseURL.Host) {
		p.data.InternalLinksCount++
		return
	}
	p.data.ExternalLinksCount++
}

func hasASCIIPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func (p *pageParser) handleText(text []byte) {
	if p.collectTitle {
		p.title.Write(text)
	}
	if p.collectFirstH1 {
		p.firstH1.Write(text)
	}
	if p.headDepth == 0 && !p.collectTitle {
		p.bodyWordCounter.Write(text)
	}
}

func (p *pageParser) handleEndTag(name []byte) {
	switch {
	case bytes.Equal(name, []byte("title")):
		p.collectTitle = false
	case bytes.Equal(name, []byte("h1")):
		p.collectFirstH1 = false
	case bytes.Equal(name, []byte("head")):
		if p.headDepth > 0 {
			p.headDepth--
		}
	}
}

func (p *pageParser) finalize() {
	p.data.Title, p.data.TitleTruncated, p.data.TitleOriginalLength = p.title.Result()
	titleLen := p.title.RuneCount()
	if titleLen == 0 {
		p.data.TitleStatus = "Missing"
	} else if titleLen < MinTitleLen {
		p.data.TitleStatus = "Too Short"
	} else if titleLen > MaxTitleLen {
		p.data.TitleStatus = "Too Long"
	} else {
		p.data.TitleStatus = "OK"
	}

	descLen := utf8.RuneCountInString(p.data.Description)
	if descLen == 0 {
		p.data.DescriptionStatus = "Missing"
	} else if descLen < MinDescriptionLen {
		p.data.DescriptionStatus = "Too Short"
	} else if descLen > MaxDescriptionLen {
		p.data.DescriptionStatus = "Too Long"
	} else {
		p.data.DescriptionStatus = "OK"
	}

	if p.data.H1Count > 0 {
		p.data.H1, p.data.H1Truncated, p.data.H1OriginalLength = p.firstH1.Result()
	} else {
		p.data.H1 = "[Missing H1]"
	}

	var subHeaders []string
	for i := 2; i <= 6; i++ {
		count := p.subHeaderCounts[i]
		if count > 0 {
			subHeaders = append(subHeaders, fmt.Sprintf("H%d:%d", i, count))
		}
	}
	if len(subHeaders) > 0 {
		p.data.H2ToH6Status = strings.Join(subHeaders, ", ")
	} else {
		p.data.H2ToH6Status = "No sub-headers (H2-H6)"
	}

	p.data.LinksCount = p.data.InternalLinksCount + p.data.ExternalLinksCount
	p.data.IsSelfCanonical = IsSelfCanonical(p.data.CanonicalURL, p.data.URL)
	p.data.WordCount = p.bodyWordCounter.Count()
}

type boundedTextCollector struct {
	builder       strings.Builder
	maxRunes      int
	storedRunes   int
	totalRunes    int
	started       bool
	pendingSpaces []rune
	pendingCount  int
}

func newBoundedTextCollector(maxRunes int) boundedTextCollector {
	return boundedTextCollector{
		maxRunes:      maxRunes,
		pendingSpaces: make([]rune, 0, min(maxRunes, 32)),
	}
}

func (c *boundedTextCollector) Write(text []byte) {
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		text = text[size:]

		if unicode.IsSpace(r) {
			if c.started {
				c.pendingCount++
				if c.storedRunes+len(c.pendingSpaces) < c.maxRunes {
					c.pendingSpaces = append(c.pendingSpaces, r)
				}
			}
			continue
		}

		if c.started && c.pendingCount > 0 {
			c.totalRunes += c.pendingCount
			for _, pending := range c.pendingSpaces {
				c.appendRune(pending)
			}
		}
		c.pendingSpaces = c.pendingSpaces[:0]
		c.pendingCount = 0
		c.started = true
		c.totalRunes++
		c.appendRune(r)
	}
}

func (c *boundedTextCollector) appendRune(r rune) {
	if c.storedRunes >= c.maxRunes {
		return
	}
	c.builder.WriteRune(r)
	c.storedRunes++
}

func (c *boundedTextCollector) Result() (string, bool, int) {
	if c.totalRunes <= c.maxRunes {
		return c.builder.String(), false, 0
	}
	return c.builder.String(), true, c.totalRunes
}

func (c *boundedTextCollector) RuneCount() int {
	return c.totalRunes
}

type wordCounter struct {
	count  int
	inWord bool
}

func (c *wordCounter) Write(text []byte) {
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		text = text[size:]
		if unicode.IsSpace(r) {
			c.inWord = false
			continue
		}
		if !c.inWord {
			c.count++
			c.inWord = true
		}
	}
}

func (c *wordCounter) Count() int {
	return c.count
}

func boundedBytes(value []byte, maxRunes int) (string, bool, int) {
	return boundedString(strings.TrimSpace(string(value)), maxRunes)
}

func boundedString(value string, maxRunes int) (string, bool, int) {
	originalLength := utf8.RuneCountInString(value)
	if originalLength <= maxRunes {
		return value, false, 0
	}

	runeCount := 0
	for index := range value {
		if runeCount == maxRunes {
			return value[:index], true, originalLength
		}
		runeCount++
	}
	return value, false, 0
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
