// Package retrieval indexes notes as embedded chunks and finds them again by
// meaning rather than keyword.
//
// Nothing here is allowed to be quietly wrong in a way the user cannot see: a
// chunk that is silently truncated by the embedder loses its tail with no error,
// and a similarity search that ignores the owner filter leaks one user's notes to
// another. Both failure modes are prevented explicitly rather than assumed away.
package retrieval

import (
	"math"
	"strings"
	"unicode"
)

// ChunkBounds are the limits a chunker works within.
type ChunkBounds struct {
	// MaxTokens is the target size of a chunk.
	MaxTokens int

	// OverlapTokens is how much of the previous chunk is repeated at the start of
	// the next one. Overlap exists because a sentence spanning a boundary would
	// otherwise be indexed half in one chunk and half in the other, and neither
	// half retrieves well.
	OverlapTokens int

	// MaxInputTokens is the embedder's own ceiling. A chunk must stay below it or
	// the embedder truncates the tail silently, which loses content with no error
	// to notice.
	MaxInputTokens int
}

// DefaultChunkBounds mirrors the shipped configuration.
func DefaultChunkBounds(maxTokens, overlap, maxInput int) ChunkBounds {
	return ChunkBounds{MaxTokens: maxTokens, OverlapTokens: overlap, MaxInputTokens: maxInput}
}

// wordsPerToken is the ratio used to approximate a model's token count without
// shipping a tokenizer.
//
// This is an approximation and is treated as one: it only has to be good enough
// that a chunk stays under the embedder's ceiling. Being wrong in that direction
// is safe (smaller chunks), being wrong the other way is not, so the ratio errs
// towards assuming more tokens than there are.
const wordsPerToken = 0.75

// ApproxTokens estimates the token count of s.
//
// Rounded up, not truncated. Truncation makes the estimate non-additive: twenty
// one-word fragments each estimate at 1 token, but the joined text of all twenty
// is 27, so a chunker budgeting on per-fragment estimates would pack twice as
// much as intended and blow the embedder's ceiling.
func ApproxTokens(s string) int {
	words := countWords(s)
	if words == 0 {
		return 0
	}

	tokens := int(math.Ceil(float64(words) / wordsPerToken))
	if tokens < 1 {
		return 1
	}

	return tokens
}

func countWords(s string) int {
	words := 0

	inWord := false

	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case !inWord:
			inWord = true

			words++
		}
	}

	return words
}

// Chunk is a slice of a note, with the token estimate it was built under.
type Chunk struct {
	Content string

	// Ordinal is the chunk's position, from zero, in reading order.
	Ordinal int

	// TokenCount is the estimate for Content, stored so the number that drove the
	// split can be inspected after the fact.
	TokenCount int
}

// Split divides text into overlapping chunks.
//
// Boundaries fall on paragraphs where possible, then sentences, then words, so a
// chunk rarely begins mid-sentence. A single unit longer than the ceiling is
// emitted on its own rather than dropped: losing content silently would be worse
// than storing an oversized chunk the embedder will truncate.
func Split(text string, bounds ChunkBounds) []Chunk {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || bounds.MaxTokens <= 0 {
		return nil
	}

	units := splitUnits(trimmed, bounds)

	chunks := pack(units, bounds)
	if len(chunks) == 0 {
		return nil
	}

	for i := range chunks {
		chunks[i].Ordinal = i
	}

	return chunks
}

// unit is an indivisible piece of text.
//
// Units are budgeted in words rather than tokens because a per-fragment token
// estimate cannot be summed safely; words convert to tokens once, at the end.
type unit struct {
	text  string
	words int
}

// splitUnits breaks text into paragraphs, then sentences within oversized
// paragraphs, then words within oversized sentences.
func splitUnits(text string, bounds ChunkBounds) []unit {
	var units []unit

	for _, paragraph := range strings.Split(text, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}

		if countWords(paragraph) <= bounds.wordBudget() {
			units = append(units, unit{text: paragraph, words: countWords(paragraph)})

			continue
		}

		units = append(units, splitSentences(paragraph, bounds)...)
	}

	return units
}

func splitSentences(paragraph string, bounds ChunkBounds) []unit {
	var units []unit

	var current strings.Builder

	flush := func() {
		if text := strings.TrimSpace(current.String()); text != "" {
			units = append(units, unit{text: text, words: countWords(text)})
		}

		current.Reset()
	}

	for _, sentence := range splitAfter(paragraph, '.', '!', '?', '\n') {
		if current.Len() > 0 && countWords(current.String()+sentence) > bounds.wordBudget() {
			flush()
		}

		current.WriteString(sentence)
	}

	flush()

	// A "sentence" with no terminator can still exceed the budget, which happens
	// with long unbroken input such as a pasted log. Split it on words rather than
	// emitting something the embedder would truncate.
	var out []unit

	for _, u := range units {
		if u.words <= bounds.wordBudget() {
			out = append(out, u)

			continue
		}

		out = append(out, splitWords(u.text, bounds)...)
	}

	return out
}

func splitWords(text string, bounds ChunkBounds) []unit {
	fields := strings.Fields(text)

	var (
		out     []unit
		current []string
	)

	flush := func() {
		if len(current) == 0 {
			return
		}

		joined := strings.Join(current, " ")

		out = append(out, unit{text: joined, words: len(current)})

		current = nil
	}

	for _, field := range fields {
		// A single word longer than the budget is emitted alone. There is no
		// smaller unit to cut it on.
		if len(current) > 0 && len(current)+1 > bounds.wordBudget() {
			flush()
		}

		current = append(current, field)
	}

	flush()

	return out
}

// splitAfter splits text after any of the given terminators, keeping the
// terminator with its sentence.
func splitAfter(text string, terminators ...rune) []string {
	var (
		out     []string
		current strings.Builder
	)

	for _, r := range text {
		current.WriteRune(r)

		for _, t := range terminators {
			if r == t {
				out = append(out, current.String())
				current.Reset()

				break
			}
		}
	}

	if rest := strings.TrimSpace(current.String()); rest != "" {
		out = append(out, rest)
	}

	return out
}

// wordBudget is how many words fit within MaxTokens.
func (b ChunkBounds) wordBudget() int {
	if b.MaxTokens <= 0 {
		return 0
	}

	return int(float64(b.MaxTokens) * wordsPerToken)
}

// overlapBudget is how many words of overlap to carry, never more than a chunk
// holds or the packer could stall.
func (b ChunkBounds) overlapBudget() int {
	overlap := b.OverlapTokens
	if overlap <= 0 {
		return 0
	}

	budget := int(float64(overlap) * wordsPerToken)
	if budget >= b.wordBudget() {
		return b.wordBudget() - 1
	}

	return budget
}

// pack fills chunks up to the word budget, carrying overlap words of the previous
// chunk forward.
func pack(units []unit, bounds ChunkBounds) []Chunk {
	budget := bounds.wordBudget()
	if budget <= 0 {
		return nil
	}

	var (
		chunks []Chunk
		buf    []unit
		words  int
	)

	flush := func() {
		if len(buf) == 0 {
			return
		}

		parts := make([]string, 0, len(buf))
		for _, u := range buf {
			parts = append(parts, u.text)
		}

		content := strings.Join(parts, "\n\n")

		chunks = append(chunks, Chunk{Content: content, TokenCount: ApproxTokens(content)})
	}

	for _, u := range units {
		// A unit bigger than the budget still gets emitted. Splitting it further is
		// the word splitter's job; it only reaches here when it could not, so
		// dropping it would lose text.
		if len(buf) > 0 && words+u.words > budget {
			flush()

			carried, carriedWords := carry(buf, bounds.overlapBudget())

			// Carrying overlap that then cannot fit the next unit would leave the
			// packer unable to make progress, so it is dropped in that case.
			if carriedWords+u.words > budget {
				carried, carriedWords = nil, 0
			}

			buf, words = carried, carriedWords
		}

		buf = append(buf, u)
		words += u.words
	}

	flush()

	return chunks
}

// carry takes up to overlap words from the tail of buf.
func carry(buf []unit, overlap int) ([]unit, int) {
	if overlap <= 0 || len(buf) == 0 {
		return nil, 0
	}

	var (
		out    []unit
		words  int
		taken  bool
		cutOff bool
	)

	for i := len(buf) - 1; i >= 0; i-- {
		if cutOff {
			break
		}

		if words+buf[i].words > overlap {
			// Take this unit anyway if it is the only candidate, so overlap makes
			// progress instead of yielding nothing on every boundary.
			if taken {
				break
			}

			cutOff = true
		}

		out = append([]unit{buf[i]}, out...)
		words += buf[i].words
		taken = true
	}

	return out, words
}
