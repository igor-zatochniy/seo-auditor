package seo

import (
	_ "embed"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

const SERPWidthModel = "liberation-sans-2.1.5:20px/14px:v1"

//go:embed fonts/LiberationSans-Regular.ttf
var serpFontBytes []byte

var serpFont, serpFontError = sfnt.Parse(serpFontBytes)

// Width is measured incrementally before storage truncation, without retaining text.
// Whitespace is collapsed; unsupported glyphs use a marked one-em estimate.
type serpWidthCounter struct {
	buffer       sfnt.Buffer
	pixels       int
	width        int64
	previous     sfnt.GlyphIndex
	started      bool
	spacePending bool
	approximate  bool
}

func (c *serpWidthCounter) Write(text []byte) {
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		text = text[size:]
		if unicode.IsSpace(r) {
			c.spacePending = c.started
			continue
		}
		if c.spacePending {
			c.add(' ')
			c.spacePending = false
		}
		c.add(r)
	}
}

func (c *serpWidthCounter) add(r rune) {
	if serpFontError != nil {
		return
	}
	scale := fixed.I(c.pixels)
	glyph, err := serpFont.GlyphIndex(&c.buffer, r)
	if err != nil || glyph == 0 {
		c.width += int64(scale)
		c.approximate = true
		c.previous = 0
		c.started = true
		return
	}
	advance, err := serpFont.GlyphAdvance(&c.buffer, glyph, scale, font.HintingNone)
	if err != nil {
		advance = scale
		c.approximate = true
	}
	if c.started && c.previous != 0 {
		if kern, err := serpFont.Kern(&c.buffer, c.previous, glyph, scale, font.HintingNone); err == nil {
			c.width += int64(kern)
		}
	}
	// Complex-script shaping is outside this fixed-font estimate.
	if unicode.Is(unicode.M, r) || unicode.Is(unicode.Arabic, r) || unicode.Is(unicode.Devanagari, r) {
		c.approximate = true
	}
	c.width += int64(advance)
	c.previous = glyph
	c.started = true
}

func (c *serpWidthCounter) Width() *int {
	if serpFontError != nil {
		return nil
	}
	width := int(max(int64(0), (c.width+63)/64))
	return &width
}

func titleWidthStatus(characters int, width *int) string {
	switch {
	case characters == 0:
		return "Missing"
	case width == nil:
		return "Not measured"
	case *width <= 580:
		return "Recommended"
	case *width <= 600:
		return "Borderline"
	default:
		return "High truncation risk"
	}
}

func descriptionWidthStatus(characters int, width *int) (desktop, mobile string) {
	switch {
	case characters == 0:
		return "Missing", "Missing"
	case width == nil:
		return "Not measured", "Not measured"
	case *width <= 680:
		return "Safe", "Safe"
	case *width <= 920:
		return "Safe", "May truncate"
	default:
		return "May truncate", "Likely truncate"
	}
}
