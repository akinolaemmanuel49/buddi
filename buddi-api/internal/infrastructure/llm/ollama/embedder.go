package ollama

import (
	"context"
	"errors"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/retrieval"
)

var (
	// ErrNoEmbedClient means the adapter was built without a runtime.
	ErrNoEmbedClient = errors.New("ollama: embedding adapter has no client")

	// ErrEmbedWidth means the runtime returned a vector of an unexpected size,
	// which would otherwise surface as an opaque constraint violation on insert.
	ErrEmbedWidth = errors.New("ollama: embedding has an unexpected width")

	// ErrEmbedWidthUnset means the adapter was built without a vector width.
	ErrEmbedWidthUnset = errors.New("ollama: embedding adapter has no dimensions configured")
)

// Embedder exposes a Client as the retrieval Embedder port.
//
// It lives beside the client rather than in the application layer for the same
// reason the planner adapter does: the port is deliberately narrower than this
// client's request type, so retrieval does not have to know the transport is
// Ollama.
//
// Embeddings and generation do not share a model here. They are separate loads on
// a machine that is already memory constrained, so the model is pinned per
// adapter and the caller is expected to set the field that matches the
// configured embedding model.
type Embedder struct {
	client *Client

	// model overrides the client default, which matters because the client's
	// default is the general model and not every runtime serves both.
	model string

	// dimensions is the requested vector width.
	dimensions int
}

// NewEmbedder wraps client for use by retrieval.
func NewEmbedder(client *Client) *Embedder {
	return &Embedder{client: client}
}

// WithModel pins the adapter to an embedding model.
func (e *Embedder) WithModel(model string) *Embedder {
	e.model = model
	return e
}

// WithDimensions requests a specific vector width.
func (e *Embedder) WithDimensions(dimensions int) *Embedder {
	e.dimensions = dimensions
	return e
}

func (e *Embedder) Embed(ctx context.Context, input string) ([]float32, error) {
	if e.client == nil {
		return nil, ErrNoEmbedClient
	}

	want := e.dimensions
	if want <= 0 {
		// No default: the vector column has a fixed width, so guessing here would
		// store embeddings that fail the insert far from the mistake.
		return nil, ErrEmbedWidthUnset
	}

	vector, err := e.client.Embed(ctx, e.model, input, want)
	if err != nil {
		return nil, err
	}

	if len(vector) != want {
		return nil, ErrEmbedWidth
	}

	return vector, nil
}

var _ retrieval.Embedder = (*Embedder)(nil)
