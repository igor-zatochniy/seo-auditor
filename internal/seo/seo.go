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

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
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
	StorageDescriptionMaxRunes = 4000
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
		RobotsHeaderDirectives(resp.Header)

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
	name              []byte
	property          []byte
	content           []byte
	rel               []byte
	href              []byte
	typeValue         []byte
	alt               []byte
	shadowRootMode    []byte
	hasName           bool
	hasProperty       bool
	hasContent        bool
	hasRel            bool
	hasHref           bool
	hasType           bool
	hasAlt            bool
	hasShadowRootMode bool
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
		case bytes.Equal(key, []byte("shadowrootmode")):
			attributes.shadowRootMode = value
			attributes.hasShadowRootMode = true
		}
		hasAttributes = moreAttributes
	}
	return attributes
}

type pageParser struct {
	data               *Data
	targetURL          *url.URL
	documentBaseURL    *url.URL
	documentBaseSet    bool
	title              boundedTextCollector
	firstH1            boundedTextCollector
	titleSeen          bool
	collectTitle       bool
	collectFirstH1     bool
	descriptionSeen    bool
	ogTitleSeen        bool
	ogDescriptionSeen  bool
	ogImageSeen        bool
	twitterCardSeen    bool
	canonicalSeen      bool
	canonicalSource    string
	metaRobots         robotsDirectiveSet
	documentPhase      documentPhase
	templateStack      []templateMode
	inertTemplateDepth int
	shadowRootDepth    int
	ignoredTextDepth   int
	ignoredTitleDepth  int
	relativeLinks      int
	subHeaderCounts    [7]int
	bodyWordCounter    wordCounter
}

type documentPhase uint8

const (
	documentPhaseHead documentPhase = iota
	documentPhaseBody
)

type templateMode uint8

const (
	templateModeInert templateMode = iota
	templateModeShadowRoot
)

func newPageParser(data *Data, targetURL string) *pageParser {
	parsedTarget, _ := url.Parse(targetURL)
	return &pageParser{
		data:            data,
		targetURL:       parsedTarget,
		documentBaseURL: parsedTarget,
		title:           newBoundedTextCollector(StorageTitleMaxRunes),
		firstH1:         newBoundedTextCollector(StorageH1MaxRunes),
	}
}

func (p *pageParser) handleStartTag(name []byte, attributes tagAttributes) {
	if bytes.Equal(name, []byte("template")) {
		p.startTemplate(attributes)
		return
	}
	if p.inertTemplateDepth > 0 {
		return
	}
	if p.documentPhase == documentPhaseHead && p.shadowRootDepth == 0 && startsBodyContent(name) {
		p.documentPhase = documentPhaseBody
	}

	switch {
	case bytes.Equal(name, []byte("title")):
		if !p.inDocumentHead() {
			p.ignoredTitleDepth++
			return
		}
		if !p.titleSeen {
			p.titleSeen = true
			p.collectTitle = true
		}
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
		if p.inDocumentHead() {
			p.handleMeta(attributes)
		} else if p.shadowRootDepth == 0 {
			p.handleRobotsMeta(attributes)
		}
	case bytes.Equal(name, []byte("base")):
		if p.inDocumentHead() && attributes.hasHref {
			p.setDocumentBase(attributes.href)
		}
	case bytes.Equal(name, []byte("link")):
		if p.inDocumentHead() && !p.canonicalSeen &&
			attributes.hasRel && attributes.hasHref &&
			hasTokenFold(attributes.rel, []byte("canonical")) {
			p.canonicalSeen = true
			p.canonicalSource = strings.TrimSpace(string(attributes.href))
			p.data.CanonicalURL, p.data.CanonicalURLTruncated, p.data.CanonicalURLOriginalLength =
				boundedBytes(attributes.href, StorageURLMaxRunes)
		}
	case bytes.Equal(name, []byte("script")):
		p.ignoredTextDepth++
		if p.shadowRootDepth == 0 && attributes.hasType &&
			bytes.EqualFold(bytes.TrimSpace(attributes.typeValue), []byte("application/ld+json")) {
			p.data.HasJsonLd = true
		}
	case bytes.Equal(name, []byte("style")):
		p.ignoredTextDepth++
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

func (p *pageParser) inDocumentHead() bool {
	return p.documentPhase == documentPhaseHead && p.shadowRootDepth == 0
}

func (p *pageParser) startTemplate(attributes tagAttributes) {
	mode := templateModeInert
	if p.documentPhase == documentPhaseBody && p.inertTemplateDepth == 0 &&
		attributes.hasShadowRootMode {
		shadowRootMode := bytes.TrimSpace(attributes.shadowRootMode)
		if bytes.EqualFold(shadowRootMode, []byte("open")) ||
			bytes.EqualFold(shadowRootMode, []byte("closed")) {
			mode = templateModeShadowRoot
		}
	}

	p.templateStack = append(p.templateStack, mode)
	if mode == templateModeShadowRoot {
		p.shadowRootDepth++
		return
	}
	p.inertTemplateDepth++
}

func (p *pageParser) endTemplate() {
	if len(p.templateStack) == 0 {
		return
	}

	lastIndex := len(p.templateStack) - 1
	mode := p.templateStack[lastIndex]
	p.templateStack = p.templateStack[:lastIndex]
	if mode == templateModeShadowRoot {
		if p.shadowRootDepth > 0 {
			p.shadowRootDepth--
		}
		return
	}
	if p.inertTemplateDepth > 0 {
		p.inertTemplateDepth--
	}
}

func (p *pageParser) setDocumentBase(rawHref []byte) {
	if p.documentBaseSet || p.targetURL == nil {
		return
	}

	parsedBase, err := url.Parse(strings.TrimSpace(string(rawHref)))
	if err != nil {
		return
	}
	resolvedBase := p.targetURL.ResolveReference(parsedBase)
	if resolvedBase.Host == "" ||
		!strings.EqualFold(resolvedBase.Scheme, "http") && !strings.EqualFold(resolvedBase.Scheme, "https") {
		return
	}

	baseCopy := *resolvedBase
	baseCopy.Fragment = ""
	p.documentBaseURL = &baseCopy
	p.documentBaseSet = true
}

func (p *pageParser) handleMeta(attributes tagAttributes) {
	p.handleRobotsMeta(attributes)

	if attributes.hasName {
		name := bytes.TrimSpace(attributes.name)
		switch {
		case bytes.EqualFold(name, []byte("description")) && !p.descriptionSeen:
			p.descriptionSeen = true
			if attributes.hasContent {
				p.data.Description, _, _ = boundedBytes(attributes.content, StorageDescriptionMaxRunes)
			}
		case bytes.EqualFold(name, []byte("twitter:card")) && !p.twitterCardSeen:
			p.twitterCardSeen = true
			if attributes.hasContent {
				p.data.TwitterCard, p.data.TwitterCardTruncated, p.data.TwitterCardOriginalLength =
					boundedBytes(attributes.content, StorageTwitterCardMaxRunes)
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
		p.data.OGDescription, _, _ = boundedBytes(attributes.content, StorageDescriptionMaxRunes)
	case bytes.EqualFold(property, []byte("og:image")) && !p.ogImageSeen:
		p.ogImageSeen = true
		p.data.OGImage, p.data.OGImageTruncated, p.data.OGImageOriginalLength =
			boundedBytes(attributes.content, StorageURLMaxRunes)
	}
}

func (p *pageParser) handleRobotsMeta(attributes tagAttributes) {
	if !attributes.hasName || !attributes.hasContent {
		return
	}

	name := bytes.TrimSpace(attributes.name)
	switch {
	case bytes.EqualFold(name, []byte("robots")):
		p.metaRobots.Add(string(attributes.content))
	case bytes.EqualFold(name, []byte("googlebot")):
		p.metaRobots.AddScoped("googlebot", string(attributes.content))
	case bytes.EqualFold(name, []byte("googlebot-news")):
		p.metaRobots.AddScoped("googlebot-news", string(attributes.content))
	}
}

func (p *pageParser) countLink(rawHref []byte) {
	href := strings.TrimSpace(string(rawHref))
	if href == "" || strings.HasPrefix(href, "#") {
		return
	}

	parsedLink, err := url.Parse(href)
	if err != nil {
		return
	}
	scheme := strings.ToLower(parsedLink.Scheme)
	if scheme != "" && scheme != "http" && scheme != "https" {
		return
	}
	if parsedLink.Host == "" {
		if scheme == "" {
			p.relativeLinks++
		}
		return
	}
	if p.targetURL != nil && sameNormalizedHost(parsedLink, p.targetURL) {
		p.data.InternalLinksCount++
		return
	}
	p.data.ExternalLinksCount++
}

func (p *pageParser) handleText(text []byte) {
	if p.inertTemplateDepth > 0 {
		return
	}
	if p.documentPhase == documentPhaseHead && p.shadowRootDepth == 0 &&
		!p.collectTitle && p.ignoredTitleDepth == 0 && p.ignoredTextDepth == 0 &&
		len(bytes.TrimSpace(text)) > 0 {
		p.documentPhase = documentPhaseBody
	}
	if p.collectTitle {
		p.title.Write(text)
	}
	if p.collectFirstH1 {
		p.firstH1.Write(text)
	}
	if p.documentPhase == documentPhaseBody && !p.collectTitle &&
		p.ignoredTextDepth == 0 && p.ignoredTitleDepth == 0 {
		p.bodyWordCounter.Write(text)
	}
}

func startsBodyContent(name []byte) bool {
	switch string(name) {
	case "base", "basefont", "bgsound", "head", "html", "link", "meta", "noframes", "noscript", "script", "style", "template", "title":
		return false
	default:
		return true
	}
}

func (p *pageParser) handleEndTag(name []byte) {
	if bytes.Equal(name, []byte("template")) {
		p.endTemplate()
		return
	}
	if p.inertTemplateDepth > 0 {
		return
	}

	switch {
	case bytes.Equal(name, []byte("title")):
		if p.collectTitle {
			p.collectTitle = false
		} else if p.ignoredTitleDepth > 0 {
			p.ignoredTitleDepth--
		}
	case bytes.Equal(name, []byte("h1")):
		p.collectFirstH1 = false
	case bytes.Equal(name, []byte("head")):
		p.documentPhase = documentPhaseBody
	case bytes.Equal(name, []byte("script")), bytes.Equal(name, []byte("style")):
		if p.ignoredTextDepth > 0 {
			p.ignoredTextDepth--
		}
	}
}

func (p *pageParser) finalize() {
	p.data.MetaRobots, p.data.MetaRobotsTruncated, p.data.MetaRobotsOriginalLength =
		p.metaRobots.Result(StorageRobotsTagMaxRunes)
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

	if p.relativeLinks > 0 {
		if p.targetURL != nil && p.documentBaseURL != nil &&
			sameNormalizedHost(p.documentBaseURL, p.targetURL) {
			p.data.InternalLinksCount += p.relativeLinks
		} else {
			p.data.ExternalLinksCount += p.relativeLinks
		}
	}
	p.data.LinksCount = p.data.InternalLinksCount + p.data.ExternalLinksCount
	p.data.IsSelfCanonical = isSelfCanonicalWithBase(
		p.canonicalSource,
		p.targetURL,
		p.documentBaseURL,
	)
	p.data.WordCount = p.bodyWordCounter.Count()
}

func hasTokenFold(value, expected []byte) bool {
	for _, token := range bytes.Fields(value) {
		if bytes.EqualFold(token, expected) {
			return true
		}
	}
	return false
}

const robotsDirectivePriorityCount = 3

type robotsDirectiveBucket struct {
	values      []string
	seen        map[string]struct{}
	storedRunes int
}

type robotsDirectiveSet struct {
	buckets        [robotsDirectivePriorityCount]robotsDirectiveBucket
	directiveCount int
	originalRunes  int
	truncated      bool
}

func (s *robotsDirectiveSet) Add(raw string) {
	s.addScoped("", raw)
}

func (s *robotsDirectiveSet) AddScoped(scope, raw string) {
	s.addScoped(strings.ToLower(strings.TrimSpace(scope)), raw)
}

func (s *robotsDirectiveSet) addScoped(scope, raw string) {
	for _, candidate := range splitRobotsDirectives(raw) {
		directive := strings.TrimSpace(candidate)
		if directive == "" {
			continue
		}
		if scope != "" {
			directive = scope + ": " + directive
		}

		priority := robotsDirectivePriority(directive)
		bucket := &s.buckets[priority]
		separatorRunes := 0
		if len(bucket.values) > 0 {
			separatorRunes = 2
		}
		remainingRunes := StorageRobotsTagMaxRunes - bucket.storedRunes - separatorRunes
		if remainingRunes <= 0 {
			s.recordTruncatedDirective(utf8.RuneCountInString(directive))
			continue
		}

		stored, truncated, originalRunes := boundedString(directive, remainingRunes)
		key := strings.ToLower(stored)
		if bucket.seen == nil {
			bucket.seen = make(map[string]struct{})
		}
		if _, exists := bucket.seen[key]; exists {
			continue
		}

		bucket.seen[key] = struct{}{}
		bucket.values = append(bucket.values, stored)
		bucket.storedRunes += separatorRunes + utf8.RuneCountInString(stored)

		globalSeparatorRunes := 0
		if s.directiveCount > 0 {
			globalSeparatorRunes = 2
		}
		if !truncated {
			originalRunes = utf8.RuneCountInString(directive)
		}
		s.originalRunes += globalSeparatorRunes + originalRunes
		s.directiveCount++
		s.truncated = s.truncated || truncated
	}
}

func (s *robotsDirectiveSet) recordTruncatedDirective(directiveRunes int) {
	separatorRunes := 0
	if s.directiveCount > 0 {
		separatorRunes = 2
	}
	s.originalRunes += separatorRunes + directiveRunes
	s.directiveCount++
	s.truncated = true
}

func splitRobotsDirectives(raw string) []string {
	var directives []string
	start := 0
	for index := 0; index < len(raw); index++ {
		if raw[index] != ',' {
			continue
		}

		current := strings.TrimSpace(raw[start:index])
		remainder := strings.TrimSpace(raw[index+1:])
		if robotsDirectiveName(current) == "unavailable_after" &&
			len(remainder) > 0 && remainder[0] >= '0' && remainder[0] <= '9' {
			continue
		}
		if current != "" {
			directives = append(directives, current)
		}
		start = index + 1
	}
	if directive := strings.TrimSpace(raw[start:]); directive != "" {
		directives = append(directives, directive)
	}
	return directives
}

func robotsDirectiveName(raw string) string {
	name := strings.TrimSpace(raw)
	if separator := strings.IndexAny(name, ":, \t\r\n"); separator >= 0 {
		name = name[:separator]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func isSupportedRobotsDirective(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "all", "index", "follow", "noindex", "nofollow", "none", "nosnippet",
		"indexifembedded", "max-snippet", "max-image-preview", "max-video-preview",
		"notranslate", "noimageindex", "unavailable_after", "noarchive", "nocache",
		"nositelinkssearchbox":
		return true
	default:
		return false
	}
}

func splitXRobotsTagScope(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	separator := strings.IndexByte(value, ':')
	if separator < 0 {
		return "", value
	}

	candidateScope := strings.TrimSpace(value[:separator])
	rules := strings.TrimSpace(value[separator+1:])
	if candidateScope == "" || rules == "" || isSupportedRobotsDirective(candidateScope) ||
		!isHTTPToken(candidateScope) || !isSupportedRobotsDirective(robotsDirectiveName(rules)) {
		return "", value
	}
	return strings.ToLower(candidateScope), rules
}

func isHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func (s *robotsDirectiveSet) String() string {
	retainedCount := 0
	for priority := range robotsDirectivePriorityCount {
		retainedCount += len(s.buckets[priority].values)
	}
	ordered := make([]string, 0, retainedCount)
	for priority := range robotsDirectivePriorityCount {
		ordered = append(ordered, s.buckets[priority].values...)
	}
	return strings.Join(ordered, ", ")
}

func (s *robotsDirectiveSet) Result(maxRunes int) (string, bool, int) {
	value, truncated, originalRunes := boundedString(s.String(), maxRunes)
	if !s.truncated {
		return value, truncated, originalRunes
	}
	return value, true, max(maxRunes+1, max(originalRunes, s.originalRunes))
}

func robotsDirectivePriority(directive string) int {
	normalized := strings.ToLower(strings.TrimSpace(directive))
	value := normalized
	if separator := strings.LastIndex(normalized, ":"); separator >= 0 {
		value = strings.TrimSpace(normalized[separator+1:])
	}
	switch value {
	case "noindex", "none":
		return 0
	case "nofollow":
		return 1
	default:
		return 2
	}
}

func RobotsHeaderDirectives(header http.Header) (string, bool, int) {
	var directives robotsDirectiveSet
	for _, value := range header.Values("X-Robots-Tag") {
		scope, rules := splitXRobotsTagScope(value)
		if scope == "" {
			directives.Add(rules)
			continue
		}
		directives.AddScoped(scope, rules)
	}
	return directives.Result(StorageRobotsTagMaxRunes)
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
	return isSelfCanonicalWithBase(canonicalURL, targetParsed, targetParsed)
}

func isSelfCanonicalWithBase(canonicalURL string, targetParsed, baseURL *url.URL) bool {
	if strings.TrimSpace(canonicalURL) == "" || targetParsed == nil || baseURL == nil {
		return false
	}
	canonicalParsed, err := url.Parse(canonicalURL)
	if err != nil {
		return false
	}
	if !canonicalParsed.IsAbs() {
		canonicalParsed = baseURL.ResolveReference(canonicalParsed)
	}
	if canonicalParsed.Host == "" ||
		!strings.EqualFold(canonicalParsed.Scheme, "http") && !strings.EqualFold(canonicalParsed.Scheme, "https") {
		return false
	}

	normalize := func(parsed *url.URL) string {
		copyValue := *parsed
		copyValue.Scheme = strings.ToLower(copyValue.Scheme)
		copyValue.Host = normalizedAuthority(&copyValue)
		copyValue.Fragment = ""
		if copyValue.Path == "" {
			copyValue.Path = "/"
		}
		return copyValue.String()
	}

	return normalize(canonicalParsed) == normalize(targetParsed)
}

func sameNormalizedHost(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return normalizedAuthority(left) == normalizedAuthority(right)
}

func normalizedAuthority(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	authority, err := crawler.NormalizeAuthority(parsed)
	if err == nil {
		return authority
	}
	return strings.ToLower(strings.TrimSuffix(parsed.Host, "."))
}
