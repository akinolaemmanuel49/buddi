package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// Embedder turns text into a vector. It is a port so the application layer does
// not depend on which runtime provides embeddings.
type Embedder interface {
	// Embed returns a vector of exactly Options.Dimensions components.
	Embed(ctx context.Context, input string) ([]float32, error)
}

// Clock is injectable for deterministic tests.
type Clock func() time.Time

// Options configures the service.
type Options struct {
	// Model names the embedding model, recorded for diagnostics only.
	Model string

	// Dimensions is the vector width, which must match the database column.
	Dimensions int

	// TopK is the default number of results returned by Search.
	TopK int

	// MinSimilarity is the cosine similarity floor. A chunk below it is not
	// returned at all, because a low-scoring "match" is worse than no context: it
	// invites the model to answer from something unrelated.
	MinSimilarity float64

	// Bounds configures chunking.
	Bounds ChunkBounds

	// EmbedTimeout bounds a single embedding call.
	EmbedTimeout time.Duration

	Clock Clock
}

func (o Options) withDefaults() Options {
	if o.TopK <= 0 {
		o.TopK = 5
	}

	if o.EmbedTimeout <= 0 {
		o.EmbedTimeout = 30 * time.Second
	}

	if o.Clock == nil {
		o.Clock = time.Now
	}

	return o
}

// Service indexes notes and searches them.
type Service struct {
	chunks domain.NoteChunkRepository
	embed  Embedder
	opts   Options
}

// NewService builds the retrieval service.
func NewService(chunks domain.NoteChunkRepository, embed Embedder, opts Options) (*Service, error) {
	if chunks == nil {
		return nil, errors.New("retrieval: chunk repository is required")
	}

	if embed == nil {
		return nil, errors.New("retrieval: embedder is required")
	}

	if opts.Dimensions <= 0 {
		return nil, errors.New("retrieval: dimensions must be positive")
	}

	if opts.MinSimilarity < -1 || opts.MinSimilarity > 1 {
		return nil, errors.New("retrieval: min similarity must be between -1 and 1")
	}

	return &Service{chunks: chunks, embed: embed, opts: opts.withDefaults()}, nil
}

// Index splits a note, embeds each chunk, and replaces the note's stored chunks.
//
// Embedding failures do not fail the note. A note is the user's data and must be
// storable even when the embedding runtime is down; the cost is that it is not
// searchable until the next successful index. Reporting that as an error instead
// would make an optional capability a hard dependency of writing.
func (s *Service) Index(ctx context.Context, note *domain.Note) error {
	if note == nil {
		return errors.New("retrieval: note is required")
	}

	pieces := Split(s.indexableText(note), s.opts.Bounds)
	if len(pieces) == 0 {
		// Nothing to search. Clear any chunks from a previous version so a note
		// that was emptied does not keep answering searches with its old text.
		return s.chunks.DeleteByNote(ctx, note.UserID, note.ID)
	}

	now := s.opts.Clock()

	chunks := make([]domain.NoteChunk, 0, len(pieces))

	for _, piece := range pieces {
		vector, err := s.embedText(ctx, piece.Content)
		if err != nil {
			return fmt.Errorf("retrieval: embed chunk %d of note %s: %w", piece.Ordinal, note.ID, err)
		}

		chunks = append(chunks, domain.NoteChunk{
			ID:         uuid.New(),
			NoteID:     note.ID,
			UserID:     note.UserID,
			Ordinal:    piece.Ordinal,
			Content:    piece.Content,
			TokenCount: piece.TokenCount,
			Embedding:  pgvector.NewVector(vector),
			CreatedAt:  now,
		})
	}

	if err := s.chunks.ReplaceChunks(ctx, chunks); err != nil {
		return err
	}

	return nil
}

// Unindex removes a note's chunks. The notes table cascades on delete, so this
// is only needed when the text is replaced without rewriting the row.
func (s *Service) Unindex(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) error {
	return s.chunks.DeleteByNote(ctx, userID, noteID)
}

// Hit is one retrieved chunk with the score that selected it.
type Hit struct {
	NoteID     uuid.UUID `json:"note_id"`
	ChunkID    uuid.UUID `json:"chunk_id"`
	Ordinal    int       `json:"ordinal"`
	Content    string    `json:"content"`
	Similarity float64   `json:"similarity"`
	Distance   float64   `json:"distance"`
}

// Search returns the user's chunks closest to query.
//
// A query with nothing to embed returns no hits rather than an error: the caller
// is usually a planner prompt that may legitimately be empty.
func (s *Service) Search(ctx context.Context, userID uuid.UUID, query string, topK int) ([]Hit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return []Hit{}, nil
	}

	if topK <= 0 {
		topK = s.opts.TopK
	}

	vector, err := s.embedText(ctx, trimmed)
	if err != nil {
		return nil, err
	}

	// The repository filters on distance, so the similarity floor is converted here
	// rather than applied twice in SQL.
	matches, err := s.chunks.Search(ctx, userID, pgvector.NewVector(vector), topK, 1-s.opts.MinSimilarity)
	if err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(matches))

	for _, match := range matches {
		hits = append(hits, Hit{
			NoteID:     match.NoteID,
			ChunkID:    match.ID,
			Ordinal:    match.Ordinal,
			Content:    match.Content,
			Similarity: match.Similarity(),
			Distance:   match.Distance,
		})
	}

	return hits, nil
}

// indexableText is what actually gets embedded for a note.
//
// The title is prepended because a note called "Rollback plan" should match a
// search for "rollback" even when the body never repeats the word. Tags are
// included for the same reason.
func (s *Service) indexableText(note *domain.Note) string {
	var b strings.Builder

	if title := strings.TrimSpace(note.Title); title != "" {
		b.WriteString(title)
		b.WriteString("\n\n")
	}

	if note.Tags.Len() > 0 {
		tags, err := note.Tags.Slice()
		if err != nil {
			// The note row itself already validated its tags, so this only fires on
			// data that never came through the service. Skipping the tags degrades
			// recall slightly; failing here would make an unsearchable note.
			tags = nil
		}

		if len(tags) > 0 {
			b.WriteString(strings.Join(tags, ", "))
			b.WriteString("\n\n")
		}
	}

	if content := strings.TrimSpace(note.Content); content != "" {
		b.WriteString(content)
	}

	return b.String()
}

func (s *Service) embedText(ctx context.Context, text string) ([]float32, error) {
	if s.opts.EmbedTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, s.opts.EmbedTimeout)
		defer cancel()
	}

	vector, err := s.embed.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	if len(vector) != s.opts.Dimensions {
		// The database column is fixed width, so this would fail much later as a
		// confusing insert error. Catching it here names the actual cause.
		return nil, fmt.Errorf(
			"retrieval: embedder returned %d dimensions, want %d (model %q)",
			len(vector), s.opts.Dimensions, s.opts.Model,
		)
	}

	return vector, nil
}
