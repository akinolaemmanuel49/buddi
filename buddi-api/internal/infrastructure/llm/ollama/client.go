// Package ollama is a thin client for a local Ollama runtime.
//
// It is deliberately hand-rolled over net/http rather than pulling in a client
// library: the API surface actually needed is four endpoints, and the project's
// rule is to avoid heavyweight abstractions. It also keeps the runtime
// replaceable, because everything the application layer sees is the small set of
// methods on Client.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// ErrSchemaWithThink is returned when a caller asks for schema-constrained
// output and disables thinking at the same time.
//
// Ollama rejects that combination outright, and it is easy to request by
// accident because both are independently reasonable choices for a fast
// structured answer. Failing here turns a confusing runtime rejection into a
// clear message at the call site.
var ErrSchemaWithThink = errors.New("ollama: structured output cannot be combined with think=false")

// ErrInvalidThinkLevel is returned for a reasoning level the runtime does not
// define.
var ErrInvalidThinkLevel = errors.New("ollama: think level must be low, medium or high")

// ErrNoEmbedding is returned when the runtime reports an empty embedding.
var ErrNoEmbedding = errors.New("ollama: runtime returned no embedding")

// Options configures a Client. Zero values are filled in with defaults that suit
// a CPU-only local runtime.
type Options struct {
	// Model is the default model for Generate when a request leaves it empty.
	Model string

	// ContextLength is sent as num_ctx. It bounds the KV cache, which is the
	// largest memory lever on a machine without a GPU.
	ContextLength int

	// Temperature is the default sampling temperature.
	Temperature float64

	// KeepAlive is how long the runtime may keep the model resident. Zero means
	// let the runtime decide.
	KeepAlive time.Duration

	// Timeout bounds a single request. CPU inference is slow, so this needs to
	// be generous; the context deadline remains the real ceiling.
	Timeout time.Duration

	// Transport overrides the HTTP transport, mainly for tests.
	Transport http.RoundTripper
}

// Client talks to a local Ollama runtime.
type Client struct {
	baseURL       string
	http          *http.Client
	model         string
	contextLength int
	temperature   float64
	keepAlive     time.Duration
}

// New builds a Client for the runtime at baseURL.
func New(baseURL string, opts Options) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("ollama: parse base url: %w", err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("ollama: base url must be http or https, got %q", parsed.Scheme)
	}

	if parsed.Host == "" {
		return nil, errors.New("ollama: base url must include a host")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}

	transport := opts.Transport
	if transport == nil {
		// Outbound calls are traced like any other dependency, so a slow model
		// call is visible in the trace rather than only in a log line.
		transport = otelhttp.NewTransport(http.DefaultTransport)
	}

	contextLength := opts.ContextLength
	if contextLength <= 0 {
		contextLength = 8192
	}

	return &Client{
		baseURL:       trimmed,
		http:          &http.Client{Transport: transport, Timeout: timeout},
		model:         opts.Model,
		contextLength: contextLength,
		temperature:   opts.Temperature,
		keepAlive:     opts.KeepAlive,
	}, nil
}

// GenerateRequest is one completion request.
type GenerateRequest struct {
	// Model overrides the client default.
	Model string

	Prompt string

	// Schema constrains the output to a JSON Schema. It must be a JSON object,
	// not a string containing JSON: Ollama parses the field as structured
	// content and rejects a quoted schema.
	Schema json.RawMessage

	// Think controls whether a reasoning model spends tokens before answering.
	// Nil leaves the runtime default. It cannot be combined with Schema.
	Think *bool

	// ThinkLevel caps reasoning effort for models that support graded thinking,
	// such as qwen3's "low", "medium" and "high".
	//
	// It exists because Think=false cannot be combined with Schema, so there was
	// otherwise no way to ask a thinking model for cheap structured output. On the
	// CPU-only runtime that matters: measured on qwen3:0.6b planning, think="low"
	// cut a call from 9.1s to 5.0s and produced a better title than the default.
	ThinkLevel string

	// NumPredict caps generated tokens. Reasoning tokens count towards this
	// budget, so a value tuned for a non-thinking model can silently produce an
	// empty response from a thinking one.
	NumPredict int

	Temperature *float64

	KeepAlive *time.Duration
}

// GenerateResponse is a completed, non-streaming generation.
type GenerateResponse struct {
	// Response is the answer with any reasoning stripped out.
	Response string

	// Thinking holds the reasoning text when the model produced any.
	Thinking string

	Model           string
	EvalCount       int
	PromptEvalCount int

	TotalDuration  time.Duration
	LoadDuration   time.Duration
	PromptDuration time.Duration
	ContextLength  int
}

// ThinkingTokens estimates how many tokens the model spent reasoning.
//
// Ollama reports a single eval_count for the whole call and does not separate
// reasoning from the answer, so this is a rough four-characters-per-token
// estimate over the reasoning text. It is only good enough to show the shape of
// the cost, which is the question being asked; do not bill against it.
func (r GenerateResponse) ThinkingTokens() int {
	if r.Thinking == "" {
		return 0
	}

	return len(r.Thinking) / 4
}

// TokensPerSecond reports generation throughput, ignoring the load and prompt
// evaluation phases so it reflects decode speed rather than cold start.
func (r GenerateResponse) TokensPerSecond() float64 {
	if r.TotalDuration <= 0 || r.EvalCount == 0 {
		return 0
	}

	decode := r.TotalDuration - r.LoadDuration - r.PromptDuration
	if decode <= 0 {
		return 0
	}

	return float64(r.EvalCount) / decode.Seconds()
}

// apiError is Ollama's error envelope.
type apiError struct {
	Error string `json:"error"`
}

type generatePayload struct {
	Model  string          `json:"model"`
	Prompt string          `json:"prompt"`
	Stream bool            `json:"stream"`
	Format json.RawMessage `json:"format,omitempty"`

	// Think is encoded rather than typed because Ollama accepts a bool or a
	// level string in the same field: true, false, or "low", "medium", "high".
	Think json.RawMessage `json:"think,omitempty"`

	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

type generateResponse struct {
	Model           string `json:"model"`
	Response        string `json:"response"`
	Thinking        string `json:"thinking"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	EvalCount       int    `json:"eval_count"`
	PromptEvalCount int    `json:"prompt_eval_count"`

	TotalDuration  int64 `json:"total_duration"`
	LoadDuration   int64 `json:"load_duration"`
	PromptDuration int64 `json:"prompt_duration"`
}

// validThinkLevels are the graded reasoning levels Ollama accepts for models
// that support them.
var validThinkLevels = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
}

// encodeThink renders the think field. A level wins over a bool because it is the
// more specific instruction, and sending both would be ambiguous on the wire.
func encodeThink(on *bool, level string) (json.RawMessage, error) {
	trimmed := strings.ToLower(strings.TrimSpace(level))

	if trimmed != "" {
		if _, ok := validThinkLevels[trimmed]; !ok {
			return nil, ErrInvalidThinkLevel
		}

		encoded, err := json.Marshal(trimmed)
		if err != nil {
			return nil, err
		}

		return encoded, nil
	}

	if on == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(*on)
	if err != nil {
		return nil, err
	}

	return encoded, nil
}

// resolve validates a generation request and renders it as a wire payload.
//
// It is shared by the buffered and streaming calls so that both reject exactly
// the same inputs. A streaming endpoint that quietly accepted something the
// buffered one refused would be a bug that only appeared under load.
func (c *Client) resolve(req GenerateRequest, stream bool) (generatePayload, error) {
	if req.Schema != nil && req.Think != nil && !*req.Think {
		return generatePayload{}, ErrSchemaWithThink
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return generatePayload{}, errors.New("ollama: prompt must not be empty")
	}

	model := req.Model
	if model == "" {
		model = c.model
	}

	if model == "" {
		return generatePayload{}, errors.New("ollama: no model configured")
	}

	if len(req.Schema) > 0 && !json.Valid(req.Schema) {
		return generatePayload{}, errors.New("ollama: schema is not valid JSON")
	}

	think, err := encodeThink(req.Think, req.ThinkLevel)
	if err != nil {
		return generatePayload{}, err
	}

	options := map[string]any{"num_ctx": c.contextLength}

	temperature := c.temperature
	if req.Temperature != nil {
		temperature = *req.Temperature
	}

	options["temperature"] = temperature

	if req.NumPredict > 0 {
		options["num_predict"] = req.NumPredict
	}

	keepAlive := c.keepAlive
	if req.KeepAlive != nil {
		keepAlive = *req.KeepAlive
	}

	payload := generatePayload{
		Model:   model,
		Prompt:  req.Prompt,
		Stream:  stream,
		Format:  req.Schema,
		Think:   think,
		Options: options,
	}

	if keepAlive > 0 {
		payload.KeepAlive = keepAlive.String()
	}

	return payload, nil
}

// Generate performs one non-streaming completion.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	payload, err := c.resolve(req, false)
	if err != nil {
		return GenerateResponse{}, err
	}

	var out generateResponse
	if err := c.post(ctx, "/api/generate", payload, &out); err != nil {
		return GenerateResponse{}, err
	}

	return out.toResponse(c.contextLength), nil
}

// StreamChunk is one incremental piece of a streamed completion.
//
// The runtime sends reasoning and answer text in separate fields, which is what
// lets a caller present the two as distinct layers instead of one interleaved
// blob. Reasoning is empty on models that do not think, and callers must treat
// that as ordinary rather than as a failure.
type StreamChunk struct {
	// Response is the newly generated answer text for this chunk.
	Response string

	// Thinking is the newly generated reasoning text for this chunk.
	Thinking string

	// Done reports that this is the final chunk, which carries the totals.
	Done bool

	// Stats is the accumulated generation, populated only when Done.
	Stats GenerateResponse
}

// GenerateStream performs a streaming completion, calling onChunk for each chunk
// as the runtime produces it.
//
// It exists because waiting for a whole answer is the wrong shape for a chat UI.
// On a CPU runtime a single answer takes tens of seconds, and a caller that sees
// nothing until the end cannot show progress or let a user cancel early.
//
// Returning an error from onChunk stops the stream. The accumulated answer is
// still carried on the final chunk's Stats, so a caller that fails mid-stream
// can salvage what it already received.
func (c *Client) GenerateStream(
	ctx context.Context,
	req GenerateRequest,
	onChunk func(StreamChunk) error,
) error {
	payload, err := c.resolve(req, true)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ollama: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(encoded),
	)
	if err != nil {
		return fmt.Errorf("ollama: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("ollama: POST /api/generate: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		if readErr != nil {
			return fmt.Errorf("ollama: %s: read error body: %w", res.Status, readErr)
		}

		return readError(res.Status, body)
	}

	var (
		answer    strings.Builder
		reasoning strings.Builder
		accum     generateResponse
	)

	// The runtime writes newline-delimited JSON objects, but decoding as a JSON
	// stream rather than scanning lines means a chunk containing a newline
	// inside a string cannot desynchronise the reader.
	decoder := json.NewDecoder(res.Body)

	for {
		var chunk generateResponse

		if err := decoder.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("ollama: decode stream: %w", err)
		}

		answer.WriteString(chunk.Response)
		reasoning.WriteString(chunk.Thinking)

		accum.Response = answer.String()
		accum.Thinking = reasoning.String()
		accum.Model = chunk.Model
		accum.EvalCount = chunk.EvalCount
		accum.PromptEvalCount = chunk.PromptEvalCount
		accum.TotalDuration = chunk.TotalDuration
		accum.LoadDuration = chunk.LoadDuration
		accum.PromptDuration = chunk.PromptDuration

		if err := onChunk(StreamChunk{
			Response: chunk.Response,
			Thinking: chunk.Thinking,
			Done:     chunk.Done,
			Stats:    accum.toResponse(c.contextLength),
		}); err != nil {
			return err
		}

		if chunk.Done {
			return nil
		}
	}

	// The stream ended without a done chunk. Anything that did arrive is still a
	// usable answer, so it is delivered as a final chunk rather than discarded.
	return onChunk(StreamChunk{Done: true, Stats: accum.toResponse(c.contextLength)})
}

// toResponse converts the wire shape into the exported one.
func (r generateResponse) toResponse(contextLength int) GenerateResponse {
	return GenerateResponse{
		Response:        r.Response,
		Thinking:        r.Thinking,
		Model:           r.Model,
		EvalCount:       r.EvalCount,
		PromptEvalCount: r.PromptEvalCount,
		TotalDuration:   time.Duration(r.TotalDuration),
		LoadDuration:    time.Duration(r.LoadDuration),
		PromptDuration:  time.Duration(r.PromptDuration),
		ContextLength:   contextLength,
	}
}

type embedPayload struct {
	Model     string `json:"model"`
	Input     string `json:"input"`
	KeepAlive string `json:"keep_alive,omitempty"`
	Truncate  *bool  `json:"truncate,omitempty"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed returns the embedding vector for input.
//
// The result is truncated to want dimensions when the model returns more, because
// the vector column is a fixed width and a mismatch is otherwise a confusing
// insert error much later. Truncation is safe for cosine similarity because the
// vector is normalised afterwards.
func (c *Client) Embed(ctx context.Context, model, input string, want int) ([]float32, error) {
	if strings.TrimSpace(input) == "" {
		return nil, errors.New("ollama: embedding input must not be empty")
	}

	if model == "" {
		model = c.model
	}

	payload := embedPayload{Model: model, Input: input}
	if c.keepAlive > 0 {
		payload.KeepAlive = c.keepAlive.String()
	}

	var out embedResponse
	if err := c.post(ctx, "/api/embed", payload, &out); err != nil {
		return nil, err
	}

	if len(out.Embeddings) == 0 || len(out.Embeddings[0]) == 0 {
		return nil, ErrNoEmbedding
	}

	vector := out.Embeddings[0]
	if want > 0 && len(vector) > want {
		vector = vector[:want]
	}

	return vector, nil
}

// Model describes one model present in the runtime.
type Model struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}

type tagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Size       int64     `json:"size"`
		ModifiedAt time.Time `json:"modified_at"`
	} `json:"models"`
}

// Tags lists the models the runtime has available.
func (c *Client) Tags(ctx context.Context) ([]Model, error) {
	var out tagsResponse
	if err := c.get(ctx, "/api/tags", &out); err != nil {
		return nil, err
	}

	models := make([]Model, 0, len(out.Models))
	for _, m := range out.Models {
		models = append(models, Model{Name: m.Name, SizeBytes: m.Size, ModifiedAt: m.ModifiedAt})
	}

	return models, nil
}

// RunningModel describes a model currently held in memory.
type RunningModel struct {
	Name          string
	SizeBytes     int64
	SizeVRAMBytes int64
	ContextLength int
}

type psResponse struct {
	Models []struct {
		Name          string `json:"name"`
		Size          int64  `json:"size"`
		SizeVRAM      int64  `json:"size_vram"`
		ContextLength int    `json:"context_length"`
	} `json:"models"`
}

// PS reports which models are resident, which is how the smoke run tells a
// keep-alive miss apart from a model that genuinely will not load.
func (c *Client) PS(ctx context.Context) ([]RunningModel, error) {
	var out psResponse
	if err := c.get(ctx, "/api/ps", &out); err != nil {
		return nil, err
	}

	running := make([]RunningModel, 0, len(out.Models))
	for _, m := range out.Models {
		running = append(running, RunningModel{
			Name:          m.Name,
			SizeBytes:     m.Size,
			SizeVRAMBytes: m.SizeVRAM,
			ContextLength: m.ContextLength,
		})
	}

	return running, nil
}

// Version returns the runtime version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}

	if err := c.get(ctx, "/api/version", &out); err != nil {
		return "", err
	}

	return out.Version, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("ollama: build request: %w", err)
	}

	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ollama: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("ollama: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()

	// Bound the read so a misbehaving runtime cannot exhaust memory.
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("ollama: read response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return readError(res.Status, body)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ollama: decode response: %w", err)
	}

	return nil
}

// readError turns a non-2xx response into an error.
//
// The runtime's own message is preferred because it is the only thing that
// explains a rejection: "think=false is not supported by this model" cannot be
// guessed from a status code, whereas a bare 500 tells a caller nothing. The body
// is passed in rather than read from the response because the buffered call site
// has already consumed it.
func readError(status string, body []byte) error {
	var envelope apiError
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
		return fmt.Errorf("ollama: %s: %w", status, errors.New(envelope.Error))
	}

	return fmt.Errorf("ollama: %s: %s", status, strings.TrimSpace(string(body)))
}
