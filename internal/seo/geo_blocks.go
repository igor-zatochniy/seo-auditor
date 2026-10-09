package seo

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

const maxGEOBlocks = 8

// Рахуємо нормалізовані Unicode-символи без штучного нарізання довгих абзаців.
type blockText struct {
	text  strings.Builder
	count int
	space bool
}

func (b *blockText) write(raw []byte) {
	for _, r := range string(raw) {
		if b.count > 500 {
			return
		}
		if unicode.IsSpace(r) {
			b.space = b.count > 0
			continue
		}
		if b.space {
			b.text.WriteByte(' ')
			b.count++
			b.space = false
		}
		b.text.WriteRune(r)
		b.count++
	}
}

func (r *geoRegion) finishBlock() {
	if !r.blockActive {
		return
	}
	r.blockActive = false
	if r.block.count < 300 || r.block.count > 500 {
		return
	}
	r.blockCount++
	if len(r.blocks) == maxGEOBlocks {
		return
	}
	r.blocks = append(r.blocks, geo.TextBlock{Heading: r.blockHeading, Text: r.block.text.String(), Characters: r.block.count})
}

func (r *geoRegion) blockStart(tag string) {
	if tag == "p" {
		r.finishBlock()
		r.block, r.blockActive, r.blockHeading = blockText{}, true, r.latestHeading
	}
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		r.headingText = blockText{}
	}
	if tag == "br" && r.blockActive {
		r.block.write([]byte(" "))
	}
}

func (r *geoRegion) blockEnd(tag string) {
	if tag == "p" {
		r.finishBlock()
	}
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		value := r.headingText.text.String()
		if utf8.RuneCountInString(value) > 200 {
			value = string([]rune(value)[:200])
		}
		r.latestHeading = value
	}
}
