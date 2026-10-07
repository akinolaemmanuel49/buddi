package ollama

import (
	"context"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
)

// Adapter exposes a Client as the planner's Generator port.
//
// It lives here rather than in the application layer because the port is
// deliberately narrower than this client's request type: the planner is the only
// consumer, and it should not have to know that the transport happens to be
// Ollama.
type Adapter struct {
	client *Client

	// model overrides the client default. Empty uses the client's own.
	model string

	// thinkLevel caps reasoning effort. It is what makes structured output
	// affordable on a CPU runtime, because a schema cannot be combined with
	// think=false.
	thinkLevel string

	// keepAlive lets a caller release the model straight after a plan instead of
	// holding it resident for the full keep-alive window.
	keepAlive time.Duration
}

// NewAdapter wraps client for use by the planner.
func NewAdapter(client *Client) *Adapter {
	return &Adapter{client: client}
}

// WithModel pins the adapter to a model, overriding the client default.
func (a *Adapter) WithModel(model string) *Adapter {
	a.model = model
	return a
}

// WithThinkLevel caps reasoning effort for calls made through this adapter.
func (a *Adapter) WithThinkLevel(level string) *Adapter {
	a.thinkLevel = level
	return a
}

// Generate satisfies planner.Generator.
//
// The schema is passed straight through. Think is never set to false, since the
// runtime rejects that alongside a schema; a think level is used instead when one
// is configured.
func (a *Adapter) Generate(ctx context.Context, req planner.Request) (planner.Response, error) {
	start := time.Now()

	res, err := a.client.Generate(ctx, GenerateRequest{
		Model:      a.model,
		Prompt:     req.Prompt,
		Schema:     req.Schema,
		ThinkLevel: a.thinkLevel,
		NumPredict: req.NumPredict,
		KeepAlive:  keepAliveOrNil(a.keepAlive),
	})
	if err != nil {
		return planner.Response{}, err
	}

	// Prefer the runtime's own timing, which excludes queueing. Fall back to wall
	// clock so a total is always recorded even if the runtime omits it.
	elapsed := res.TotalDuration
	if elapsed <= 0 {
		elapsed = time.Since(start)
	}

	return planner.Response{
		Text:           res.Response,
		Reasoning:      res.Thinking,
		ThinkingTokens: res.ThinkingTokens(),
		Elapsed:        elapsed,
		Model:          res.Model,
	}, nil
}

// GenerateStream satisfies planner.StreamingGenerator.
//
// It exists so a caller watching a plan arrive can see it being written. The
// runtime's two output fields are forwarded separately for that reason: reasoning
// and answer are different things to a reader even when the model emits them
// interleaved.
func (a *Adapter) GenerateStream(
	ctx context.Context,
	req planner.Request,
	onDelta func(planner.Delta) error,
) (planner.Response, error) {
	start := time.Now()

	var res GenerateResponse

	err := a.client.GenerateStream(ctx, GenerateRequest{
		Model:      a.model,
		Prompt:     req.Prompt,
		Schema:     req.Schema,
		ThinkLevel: a.thinkLevel,
		NumPredict: req.NumPredict,
		KeepAlive:  keepAliveOrNil(a.keepAlive),
	}, func(chunk StreamChunk) error {
		// The accumulated text arrives on the final chunk, not as a return value:
		// GenerateStream reports only an error, because the answer is whatever the
		// deltas added up to. Without this the plan would be parsed from an empty
		// string and every call would quietly fall back.
		if chunk.Done {
			res = chunk.Stats
		}

		if chunk.Response == "" && chunk.Thinking == "" {
			return nil
		}

		return onDelta(planner.Delta{Text: chunk.Response, Reasoning: chunk.Thinking})
	})
	if err != nil {
		return planner.Response{}, err
	}

	// The runtime's own timing excludes queueing, which is the number worth
	// reporting; the wall clock is the fallback so a total always exists.
	elapsed := res.TotalDuration
	if elapsed <= 0 {
		elapsed = time.Since(start)
	}

	return planner.Response{
		Text:           res.Response,
		Reasoning:      res.Thinking,
		ThinkingTokens: res.ThinkingTokens(),
		Elapsed:        elapsed,
		Model:          res.Model,
	}, nil
}

// keepAliveOrNil returns nil when no keep-alive is configured, which lets the
// runtime apply its own default rather than being told to unload immediately.
func keepAliveOrNil(d time.Duration) *time.Duration {
	if d <= 0 {
		return nil
	}

	return &d
}

// ChatAdapter exposes the client as the chat layer's Streamer.
//
// It is a distinct type from Adapter rather than an extra method on it, because the
// two want different defaults. The planner is bounded by a schema and asks for
// reasoning; a chat reply is free-form prose and would rather spend the whole token
// budget on the answer.
type ChatAdapter struct {
	client *Client

	model      string
	thinkLevel string
}

// NewChatAdapter wraps client for conversational replies.
func NewChatAdapter(client *Client) *ChatAdapter {
	return &ChatAdapter{client: client}
}

// WithModel pins the adapter to a model, overriding the client default.
func (a *ChatAdapter) WithModel(model string) *ChatAdapter {
	a.model = model
	return a
}

// WithThinkLevel caps reasoning effort. Empty, the default here, is the right
// setting for a non-thinking model: it exposes none to cap.
func (a *ChatAdapter) WithThinkLevel(level string) *ChatAdapter {
	a.thinkLevel = level
	return a
}

// Stream satisfies chat.Streamer.
func (a *ChatAdapter) Stream(
	ctx context.Context,
	req chat.StreamRequest,
	onDelta func(chat.Delta) error,
) (chat.StreamResult, error) {
	start := time.Now()

	var res GenerateResponse

	err := a.client.GenerateStream(ctx, GenerateRequest{
		Model:      a.model,
		Prompt:     req.Prompt,
		ThinkLevel: a.thinkLevel,
		NumPredict: req.NumPredict,
	}, func(chunk StreamChunk) error {
		// As above: the accumulated reply is delivered on the final chunk, since a
		// stream reports only an error and a sequence of deltas.
		if chunk.Done {
			res = chunk.Stats
		}

		if chunk.Response == "" && chunk.Thinking == "" {
			return nil
		}

		return onDelta(chat.Delta{Text: chunk.Response, Reasoning: chunk.Thinking})
	})
	if err != nil {
		return chat.StreamResult{}, err
	}

	elapsed := res.TotalDuration
	if elapsed <= 0 {
		elapsed = time.Since(start)
	}

	return chat.StreamResult{
		Text:      res.Response,
		Reasoning: res.Thinking,
		Model:     res.Model,
		Elapsed:   elapsed,
	}, nil
}
