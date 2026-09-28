package tiktoken

import (
	"math"
	"unicode/utf8"
)

// Byte classes for the ASCII scanner. The classifier is a 256-entry table rather
// than a chain of comparisons because Add runs once per streamed frame for the
// whole life of an answer: the previous byte-by-byte word and whitespace
// checks cost seven branches per byte, and a table costs one load and one
// compare.
const (
	classWord     uint8 = iota // [A-Za-z0-9]: opens or continues a word run
	classSpace                 // ' ', '\t', '\n', '\r': closes a run, adds no token
	classPunct                 // any other ASCII byte: closes a run and adds its own token
	classNonASCII              // lead byte of a multi-byte rune: 1.5 tokens
)

// asciiClass maps every byte value to its class. Bytes >= utf8.RuneSelf carry
// classNonASCII, which routes them to the rune decoder.
var asciiClass = func() (table [256]uint8) {
	for b := 0; b < 256; b++ {
		switch {
		case b >= utf8.RuneSelf:
			table[b] = classNonASCII
		case (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9'):
			table[b] = classWord
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			table[b] = classSpace
		default:
			table[b] = classPunct
		}
	}
	return table
}()

type Estimator struct {
	tokens float64
	inWord bool
}

// Add folds one more fragment into the running estimate.
//
// The estimator is a counting machine: it emits one token when a word run ends,
// one for each non-space ASCII punctuation byte, and 1.5 for each non-ASCII
// rune. A word run therefore needs no per-byte accounting at all, only its end,
// so the inner loop below consumes a whole run with nothing but a load, a
// compare and an increment — the flush branch and the class dispatch are hoisted
// out of the one loop that dominates streaming text.
func (e *Estimator) Add(text string) {
	n := len(text)
	i := 0
	for i < n {
		switch asciiClass[text[i]] {
		case classWord:
			e.inWord = true
			i++
			for i < n && asciiClass[text[i]] == classWord {
				i++
			}
		case classSpace:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			i++
		case classPunct:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			e.tokens++
			i++
		default:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			_, size := utf8.DecodeRuneInString(text[i:])
			e.tokens += 1.5
			i += size
		}
	}
}

// AddBytes is Add over a byte slice, for callers that already hold the payload
// as bytes. It mirrors Add exactly, including the word-run fast path.
func (e *Estimator) AddBytes(text []byte) {
	n := len(text)
	i := 0
	for i < n {
		switch asciiClass[text[i]] {
		case classWord:
			e.inWord = true
			i++
			for i < n && asciiClass[text[i]] == classWord {
				i++
			}
		case classSpace:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			i++
		case classPunct:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			e.tokens++
			i++
		default:
			if e.inWord {
				e.tokens++
				e.inWord = false
			}
			_, size := utf8.DecodeRune(text[i:])
			e.tokens += 1.5
			i += size
		}
	}
}

func (e *Estimator) Count() int {
	tokens := e.tokens
	if e.inWord {
		tokens += 1
	}
	return int(math.Round(tokens))
}

func (e *Estimator) Reset() {
	e.tokens = 0
	e.inWord = false
}

// EstimateTextTokens estimates token count for mixed ASCII and CJK text.
func EstimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	var estimator Estimator
	estimator.Add(text)
	return estimator.Count()
}
