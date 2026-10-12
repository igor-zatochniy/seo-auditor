package duplicates

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
	"strings"
	"unicode"

	"github.com/mfonda/simhash"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const Model = "main-text-shingle5-simhash64-v1"
const MinWords = 80

type Signature struct {
	Model    string `json:"model"`
	SHA256   string `json:"sha256"`
	SimHash  string `json:"simhash"`
	Words    int    `json:"words"`
	Source   string `json:"source"`
	Language string `json:"language,omitempty"`
	Complete bool   `json:"complete"`
}

// Collector зберігає лише поточне слово, п'ять слів шингла та вектор SimHash.
type Collector struct {
	exact      hash.Hash
	fold       cases.Caser
	word       []rune
	window     [5]string
	vector     simhash.Vector
	words      int
	incomplete bool
	overlong   bool
}

func NewCollector() Collector {
	return Collector{exact: sha256.New(), fold: cases.Fold()}
}

func (c *Collector) Write(text []byte) {
	for _, r := range string(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			if len(c.word) < 128 && !c.overlong {
				c.word = append(c.word, r)
			} else {
				c.incomplete, c.overlong = true, true
			}
		} else {
			c.Boundary()
			if !unicode.IsSpace(r) {
				// Символи та пунктуація важливі для точного збігу, зокрема валюти.
				value := c.fold.String(norm.NFKC.String(string(r)))
				_, _ = c.exact.Write([]byte(value))
				_, _ = c.exact.Write([]byte{0})
			}
		}
	}
}

func (c *Collector) Boundary() {
	if len(c.word) == 0 && !c.overlong {
		return
	}
	if !c.overlong {
		word := c.fold.String(norm.NFKC.String(string(c.word)))
		_, _ = c.exact.Write([]byte(word))
		_, _ = c.exact.Write([]byte{0})
		copy(c.window[:], c.window[1:])
		c.window[4] = word
		c.words++
		if c.words >= len(c.window) {
			v := simhash.VectorizeBytes([][]byte{[]byte(strings.Join(c.window[:], " "))})
			for i := range c.vector {
				c.vector[i] += v[i]
			}
		}
	}
	c.word = c.word[:0]
	c.overlong = false
}

func (c *Collector) Result(source, language string, complete bool) *Signature {
	c.Boundary()
	return &Signature{
		Model: Model, SHA256: hex.EncodeToString(c.exact.Sum(nil)),
		SimHash: strconv.FormatUint(simhash.Fingerprint(c.vector), 16),
		Words:   c.words, Source: source, Language: language,
		Complete: complete && !c.incomplete,
	}
}
