package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
)

func testClient(t *testing.T, handler http.HandlerFunc) *ollama.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := ollama.New(server.URL, ollama.Options{
		Model:         "test-model",
		ContextLength: 4096,
		Temperature:   0.1,
		KeepAlive:     90 * time.Second,
		Timeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client
}

func TestNewRejectsUnusableBaseURLs(t *testing.T) {
	for _, base := range []string{"", "127.0.0.1:11434", "ftp://host", "://"} {
		if _, err := ollama.New(base, ollama.Options{}); err == nil {
			t.Errorf("New(%q) accepted an unusable base url", base)
		}
	}
}

func TestGenerateSendsSchemaAsNestedObject(t *testing.T) {
	var received map[string]json.RawMessage

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode: %v", err)
		}

		w.Write([]byte(`{"model":"test-model","response":"{}","done":true,"eval_count":3,"prompt_eval_count":9}`))
	})

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{
		Prompt: "plan this",
		Schema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Ollama parses the format field as structured content, so a quoted schema
	// is rejected at runtime. Passing it as a JSON string was a real bug, so the
	// shape is asserted here rather than left to the live runtime.
	raw, ok := received["format"]
	if !ok {
		t.Fatal("request had no format field")
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("format was not a JSON object, got %s", raw)
	}

	if decoded["type"] != "object" {
		t.Errorf("format.type = %v, want object", decoded["type"])
	}
}

// A think level must reach the runtime as a JSON string in the same field that
// takes a bool, not as a separate key Ollama would ignore. Without it there is no
// way to get cheap structured output from a reasoning model, since think=false is
// rejected alongside a schema.
func TestGenerateSendsThinkLevelAsAString(t *testing.T) {
	var received map[string]json.RawMessage

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode: %v", err)
		}

		w.Write([]byte(`{"model":"test-model","response":"{}","done":true}`))
	})

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{
		Prompt:     "plan this",
		Schema:     json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
		ThinkLevel: "low",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	raw, ok := received["think"]
	if !ok {
		t.Fatal("request had no think field")
	}

	if string(raw) != `"low"` {
		t.Errorf("think = %s, want the string \"low\"", raw)
	}
}

// A level is sent alongside a schema, which is the whole point of it, so this
// must be accepted even though think=false with a schema is not.
func TestGenerateAllowsThinkLevelWithSchema(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"model":"test-model","response":"{}","done":true}`))
	})

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{
		Prompt:     "plan this",
		Schema:     json.RawMessage(`{"type":"object"}`),
		ThinkLevel: "low",
	})
	if err != nil {
		t.Errorf("Generate: %v", err)
	}
}

func TestGenerateRejectsAnUnknownThinkLevel(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should reach the runtime")
	})

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{
		Prompt:     "plan this",
		ThinkLevel: "minimal",
	})
	if !errors.Is(err, ollama.ErrInvalidThinkLevel) {
		t.Errorf("err = %v, want ErrInvalidThinkLevel", err)
	}
}

// A thinking model spends part of its token budget on reasoning, so a cap tuned
// for a non-thinking model can return nothing at all. The client must not
// silently present that as an empty answer.
func TestGenerateReportsThinkingSeparately(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":"Paris","thinking":"weighing options","eval_count":40,"prompt_eval_count":5,"done":true}`))
	})

	res, err := client.Generate(context.Background(), ollama.GenerateRequest{Prompt: "capital of France?"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.Response != "Paris" {
		t.Errorf("Response = %q, want Paris", res.Response)
	}

	if res.Thinking == "" {
		t.Error("Thinking was dropped, so reasoning cost would be invisible")
	}

	if res.EvalCount != 40 {
		t.Errorf("EvalCount = %d, want 40", res.EvalCount)
	}
}

func TestGenerateRejectsSchemaWithThinkDisabled(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the runtime, but it should have been rejected first")
	})

	think := false

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{
		Prompt: "plan this",
		Schema: json.RawMessage(`{"type":"object"}`),
		Think:  &think,
	})

	if !errors.Is(err, ollama.ErrSchemaWithThink) {
		t.Errorf("err = %v, want ErrSchemaWithThink", err)
	}
}

func TestGenerateRejectsUnusableRequests(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the runtime, but it should have been rejected first")
	})

	cases := []struct {
		name string
		req  ollama.GenerateRequest
	}{
		{name: "blank prompt", req: ollama.GenerateRequest{Prompt: "   "}},
		{name: "invalid schema", req: ollama.GenerateRequest{Prompt: "x", Schema: json.RawMessage(`{oops`)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.Generate(context.Background(), tc.req); err == nil {
				t.Error("Generate accepted an invalid request")
			}
		})
	}
}

func TestGenerateSurfacesRuntimeErrorEnvelope(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model \"nope\" not found, try pulling it first"}`))
	})

	_, err := client.Generate(context.Background(), ollama.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("Generate ignored a 404")
	}

	if !strings.Contains(err.Error(), "try pulling it first") {
		t.Errorf("err = %v, want the runtime's own explanation", err)
	}
}

func TestEmbedTruncatesToRequestedWidth(t *testing.T) {
	wide := make([]float32, 1024)
	for i := range wide {
		wide[i] = 0.01
	}

	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{wide}})
	})

	vector, err := client.Embed(context.Background(), "embed-model", "some text", 768)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(vector) != 768 {
		t.Errorf("len(vector) = %d, want 768 to match the vector column", len(vector))
	}
}

func TestEmbedRejectsEmptyResult(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[]}`))
	})

	if _, err := client.Embed(context.Background(), "embed-model", "text", 768); !errors.Is(err, ollama.ErrNoEmbedding) {
		t.Errorf("err = %v, want ErrNoEmbedding", err)
	}
}

func TestEmbedRejectsBlankInput(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the runtime, but it should have been rejected first")
	})

	if _, err := client.Embed(context.Background(), "embed-model", "  ", 768); err == nil {
		t.Error("Embed accepted blank input")
	}
}

func TestTagsAndPSDecode(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:0.6b","size":522000000,"modified_at":"2026-10-04T00:00:00Z"}]}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:0.6b","size":700000000,"size_vram":0,"context_length":8192}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	models, err := client.Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if len(models) != 1 || models[0].Name != "qwen3:0.6b" {
		t.Fatalf("Tags = %+v", models)
	}

	running, err := client.PS(context.Background())
	if err != nil {
		t.Fatalf("PS: %v", err)
	}

	if len(running) != 1 || running[0].ContextLength != 8192 {
		t.Fatalf("PS = %+v", running)
	}
}
