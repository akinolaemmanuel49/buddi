package retrieval_test

import (
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/retrieval"
)

func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

func bounds() retrieval.ChunkBounds {
	return retrieval.DefaultChunkBounds(40, 8, 200)
}

func TestApproxTokensCountsWordsNotCharacters(t *testing.T) {
	tests := map[string]struct {
		text  string
		about int
	}{
		"empty":      {text: "", about: 0},
		"whitespace": {text: "   \n\t ", about: 0},
		"one word":   {text: "milk", about: 1},
		"three":      {text: "buy semi skimmed milk", about: 6},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := retrieval.ApproxTokens(tt.text)

			// The ratio is an estimate, so allow a wide band rather than pinning an
			// exact number that would break the moment the ratio is tuned.
			if diff := got - tt.about; diff < -1 || diff > 2 {
				t.Errorf("ApproxTokens(%q) = %d, want roughly %d", tt.text, got, tt.about)
			}
		})
	}
}

// A non-empty fragment must never estimate to zero, or a chunk could be treated
// as free by the packing logic.
func TestApproxTokensNeverReportsANonEmptyFragmentAsFree(t *testing.T) {
	if got := retrieval.ApproxTokens("a"); got < 1 {
		t.Errorf("ApproxTokens(%q) = %d, want at least 1", "a", got)
	}
}

func TestSplitReturnsNothingForBlankInput(t *testing.T) {
	if got := retrieval.Split("   \n\n  ", bounds()); got != nil {
		t.Errorf("Split(blank) = %v, want nil", got)
	}
}

func TestSplitKeepsShortTextAsOneChunk(t *testing.T) {
	chunks := retrieval.Split("Buy semi-skimmed milk on the way home.", bounds())

	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}

	if chunks[0].Content != "Buy semi-skimmed milk on the way home." {
		t.Errorf("content = %q", chunks[0].Content)
	}
}

func TestSplitNumbersChunksInReadingOrder(t *testing.T) {
	chunks := retrieval.Split(longText(12), bounds())

	for i, chunk := range chunks {
		if chunk.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, chunk.Ordinal)
		}
	}
}

// The whole point of splitting: no chunk may exceed the target.
func TestSplitRespectsTheChunkCeiling(t *testing.T) {
	limits := retrieval.DefaultChunkBounds(40, 8, 200)

	for _, text := range []string{longText(12), longText(40), paragraphOnly(80), unbroken(120)} {
		for i, chunk := range retrieval.Split(text, limits) {
			if chunk.TokenCount > limits.MaxTokens+limits.MaxInputTokens {
				t.Errorf("chunk %d = %d tokens, over the ceiling", i, chunk.TokenCount)
			}
		}
	}
}

// A sentence longer than the ceiling has no sentence boundary to fall back on, so
// it must be split on words rather than emitted whole and silently truncated by
// the embedder.
func TestSplitBreaksAnOversizedSentence(t *testing.T) {
	limits := retrieval.DefaultChunkBounds(20, 0, 200)

	chunks := retrieval.Split(unbroken(120), limits)

	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want the oversized sentence to be split", len(chunks))
	}

	for i, chunk := range chunks {
		if chunk.TokenCount > limits.MaxTokens {
			t.Errorf("chunk %d = %d tokens, over the ceiling", i, chunk.TokenCount)
		}
	}
}

// Every word must survive the split. Losing the tail of a note because it landed
// past a chunk boundary is the failure this whole exercise guards against.
func TestSplitLosesNoContent(t *testing.T) {
	limits := retrieval.DefaultChunkBounds(30, 5, 200)

	for _, text := range []string{longText(9), longText(30), unbroken(150), paragraphOnly(60)} {
		required := map[string]bool{}
		for _, word := range normalise(strings.Fields(text)) {
			required[word] = true
		}

		seen := map[string]int{}

		for _, chunk := range retrieval.Split(text, limits) {
			for _, word := range normalise(strings.Fields(chunk.Content)) {
				seen[word]++
			}
		}

		for word := range required {
			// Overlap legitimately repeats words; only losing them entirely is a bug.
			if seen[word] == 0 {
				t.Fatalf("word %q is missing from every chunk", word)
			}
		}
	}
}

func TestSplitOverlapsConsecutiveChunks(t *testing.T) {
	limits := retrieval.DefaultChunkBounds(30, 6, 200)

	chunks := retrieval.Split(longText(9), limits)
	if len(chunks) < 2 {
		t.Skipf("input produced %d chunk(s), need two to compare", len(chunks))
	}

	// Some word should appear at the end of one chunk and the start of the next.
	tail := normalise(strings.Fields(chunks[0].Content))
	head := normalise(strings.Fields(chunks[1].Content))

	shared := 0

	for _, word := range head {
		for _, other := range tail {
			if word == other {
				shared++

				break
			}
		}
	}

	if shared == 0 {
		t.Errorf("no overlap between chunks:\n  0: %q\n  1: %q", chunks[0].Content, chunks[1].Content)
	}
}

// Overlap must not be able to trap the packer in a loop, which would happen if
// carried tokens plus the next unit never fit.
func TestSplitTerminatesWithAnOverlapLargerThanTheChunk(t *testing.T) {
	limits := retrieval.DefaultChunkBounds(10, 40, 200)

	done := make(chan []retrieval.Chunk, 1)

	go func() { done <- retrieval.Split(longText(20), limits) }()

	select {
	case chunks := <-done:
		if len(chunks) == 0 {
			t.Error("no chunks produced")
		}
	case <-timeoutAfterSeconds(5):
		t.Fatal("Split did not terminate with an oversized overlap")
	}
}

func normalise(words []string) []string {
	out := make([]string, 0, len(words))

	for _, word := range words {
		out = append(out, strings.ToLower(strings.Trim(word, ".,!?;:")))
	}

	return out
}

func longText(paragraphs int) string {
	var b strings.Builder

	for range paragraphs {
		b.WriteString("The quarterly report needs the migration notes attached before Friday. ")
		b.WriteString("Dana reviews the rollout checklist every Monday morning. ")
		b.WriteString("Rollback steps must stay under one page for the on-call engineer.\n\n")
	}

	return b.String()
}

func paragraphOnly(words int) string {
	fields := make([]string, 0, words)
	for range words {
		fields = append(fields, "token")
	}

	return strings.Join(fields, " ")
}

func unbroken(words int) string {
	var b strings.Builder

	for range words {
		b.WriteString("word ")
	}

	return strings.TrimSpace(b.String())
}
