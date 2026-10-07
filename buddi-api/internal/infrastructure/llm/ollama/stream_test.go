package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
)

// writeNDJSON writes newline-delimited chunks the way the runtime streams them,
// flushing between each so the handler under test observes them separately
// rather than as one buffered body.
func writeNDJSON(t *testing.T, w http.ResponseWriter, chunks ...string) {
	t.Helper()

	for _, chunk := range chunks {
		if _, err := io.WriteString(w, chunk+"\n"); err != nil {
			return
		}

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func TestGenerateStreamDeliversDeltasAsTheyArrive(t *testing.T) {
	var received map[string]json.RawMessage

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode: %v", err)
		}

		writeNDJSON(t, w,
			`{"model":"test-model","thinking":"weighing","done":false}`,
			`{"model":"test-model","thinking":"options","done":false}`,
			`{"model":"test-model","response":"Buy ","done":false}`,
			`{"model":"test-model","response":"milk","done":false}`,
			`{"model":"test-model","done":true,"eval_count":4,"prompt_eval_count":11,"total_duration":9000000000}`,
		)
	})

	var (
		answerParts    []string
		reasoningParts []string
		final          ollama.GenerateResponse
	)

	err := client.GenerateStream(context.Background(), ollama.GenerateRequest{
		Prompt: "plan this",
		Schema: json.RawMessage(`{"type":"object"}`),
	}, func(chunk ollama.StreamChunk) error {
		if chunk.Response != "" {
			answerParts = append(answerParts, chunk.Response)
		}

		if chunk.Thinking != "" {
			reasoningParts = append(reasoningParts, chunk.Thinking)
		}

		if chunk.Done {
			final = chunk.Stats
		}

		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}

	// Streaming must be requested explicitly, or the runtime sends one buffered
	// object and the delta callback never fires.
	if string(received["stream"]) != "true" {
		t.Errorf("stream = %s, want true", received["stream"])
	}

	if got := strings.Join(answerParts, ""); got != "Buy milk" {
		t.Errorf("answer deltas joined = %q, want %q", got, "Buy milk")
	}

	// Reasoning and answer are separate layers; merging them would lose the
	// distinction the UI exists to show.
	if got := strings.Join(reasoningParts, ""); got != "weighingoptions" {
		t.Errorf("reasoning deltas joined = %q, want %q", got, "weighingoptions")
	}

	if final.Response != "Buy milk" {
		t.Errorf("final answer = %q, want the accumulated text", final.Response)
	}

	if final.Thinking != "weighingoptions" {
		t.Errorf("final reasoning = %q, want the accumulated text", final.Thinking)
	}

	if final.EvalCount != 4 || final.PromptEvalCount != 11 {
		t.Errorf("totals = %d/%d, want 4/11", final.EvalCount, final.PromptEvalCount)
	}
}

// A stream that ends without a done chunk still produced usable text, so it must
// be delivered rather than dropped.
func TestGenerateStreamDeliversATrailingFinalChunkWithoutDone(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeNDJSON(t, w, `{"model":"test-model","response":"partial"}`)
	})

	var final ollama.GenerateResponse

	err := client.GenerateStream(context.Background(), ollama.GenerateRequest{Prompt: "hi"},
		func(chunk ollama.StreamChunk) error {
			if chunk.Done {
				final = chunk.Stats
			}

			return nil
		})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}

	if final.Response != "partial" {
		t.Errorf("final answer = %q, want the text that did arrive", final.Response)
	}
}

// A chat client that disconnects must stop the generation rather than have the
// model keep producing tokens nobody will read.
func TestGenerateStreamStopsWhenTheCallbackFails(t *testing.T) {
	var chunks int

	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeNDJSON(t, w,
			`{"model":"test-model","response":"a","done":false}`,
			`{"model":"test-model","response":"b","done":false}`,
			`{"model":"test-model","response":"c","done":true}`,
		)
	})

	sentinel := errors.New("client went away")

	err := client.GenerateStream(context.Background(), ollama.GenerateRequest{Prompt: "hi"},
		func(chunk ollama.StreamChunk) error {
			chunks++

			if chunks == 2 {
				return sentinel
			}

			return nil
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the callback error", err)
	}

	if chunks != 2 {
		t.Errorf("callback ran %d times, want 2: the stream must stop on failure", chunks)
	}
}

// A rejected request must be refused before a connection is opened, and with the
// same error the buffered call returns, so both paths reject identically.
func TestGenerateStreamValidatesBeforeDialling(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the runtime, but it should have been rejected first")
	})

	think := false

	err := client.GenerateStream(context.Background(), ollama.GenerateRequest{
		Prompt: "hi",
		Schema: json.RawMessage(`{"type":"object"}`),
		Think:  &think,
	}, func(ollama.StreamChunk) error { return nil })
	if !errors.Is(err, ollama.ErrSchemaWithThink) {
		t.Errorf("err = %v, want ErrSchemaWithThink", err)
	}
}

// The runtime's explanation is the only way a caller learns why a stream was
// refused, so it has to survive the streaming path too.
func TestGenerateStreamSurfacesRuntimeErrorEnvelope(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"registry.ollama.chat: model requires more system memory"}`))
	})

	err := client.GenerateStream(context.Background(), ollama.GenerateRequest{Prompt: "hi"},
		func(ollama.StreamChunk) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "requires more system memory") {
		t.Errorf("err = %v, want the runtime's own explanation", err)
	}
}
