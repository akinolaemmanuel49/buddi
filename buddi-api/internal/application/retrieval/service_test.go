package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/retrieval"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

const testWidth = 4

// fakeEmbedder returns a vector derived from the text, so a search for the same
// words lands nearest the chunks containing them.
type fakeEmbedder struct {
	vector []float32
	err    error

	inputs []string
	// widths records the width of each returned vector, to prove the adapter is
	// asked for the configured size.
	widths []int
}

func (f *fakeEmbedder) Embed(_ context.Context, input string) ([]float32, error) {
	f.inputs = append(f.inputs, input)

	if f.err != nil {
		return nil, f.err
	}

	if f.vector != nil {
		f.widths = append(f.widths, len(f.vector))

		return f.vector, nil
	}

	vector := make([]float32, testWidth)
	for i, word := range strings.Fields(strings.ToLower(input)) {
		vector[i%testWidth] += float32(len(word))
	}

	f.widths = append(f.widths, len(vector))

	return vector, nil
}

type fakeChunkStore struct {
	replaced [][]domain.NoteChunk
	deleted  []uuid.UUID
	searched []domain.Vector

	limit       int
	minDistance float64

	searchResult []domain.NoteChunkMatch
	searchErr    error
}

func (s *fakeChunkStore) ReplaceChunks(_ context.Context, chunks []domain.NoteChunk) error {
	s.replaced = append(s.replaced, chunks)
	return nil
}

func (s *fakeChunkStore) ListByNote(context.Context, uuid.UUID, uuid.UUID) ([]domain.NoteChunk, error) {
	return nil, nil
}

func (s *fakeChunkStore) DeleteByNote(_ context.Context, _ uuid.UUID, noteID uuid.UUID) error {
	s.deleted = append(s.deleted, noteID)
	return nil
}

func (s *fakeChunkStore) Search(
	_ context.Context,
	_ uuid.UUID,
	query domain.Vector,
	limit int,
	minDistance float64,
) ([]domain.NoteChunkMatch, error) {
	s.searched = append(s.searched, query)
	s.limit, s.minDistance = limit, minDistance

	return s.searchResult, s.searchErr
}

func newService(t *testing.T, store *fakeChunkStore, embed *fakeEmbedder, mutate ...func(*retrieval.Options)) *retrieval.Service {
	t.Helper()

	opts := retrieval.Options{
		Model:         "test-embed",
		Dimensions:    testWidth,
		TopK:          3,
		MinSimilarity: 0.3,
		Bounds:        retrieval.DefaultChunkBounds(40, 8, 200),
		Clock:         func() time.Time { return time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC) },
	}

	for _, m := range mutate {
		m(&opts)
	}

	service, err := retrieval.NewService(store, embed, opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

func aNote() *domain.Note {
	note, err := domain.NewNote(
		uuid.New(),
		"Rollback plan",
		"The rollback steps must stay under one page for the on-call engineer. "+
			"Dana reviews the rollout checklist every Monday morning.",
		[]string{"oncall", "release"},
		domain.NoteSourceManual,
		time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		panic(err)
	}

	return note
}

func TestNewServiceValidatesOptions(t *testing.T) {
	// Held as interface values so a nil here really is nil. A typed nil pointer in
	// an interface is not, which would make these cases pass validation for the
	// wrong reason.
	tests := map[string]struct {
		store domain.NoteChunkRepository
		embed retrieval.Embedder
		opts  retrieval.Options
	}{
		"no store":    {store: nil, embed: &fakeEmbedder{}, opts: retrieval.Options{Dimensions: 4}},
		"no embedder": {store: &fakeChunkStore{}, embed: nil, opts: retrieval.Options{Dimensions: 4}},
		"no dimensions": {
			store: &fakeChunkStore{}, embed: &fakeEmbedder{}, opts: retrieval.Options{},
		},
		"similarity too high": {
			store: &fakeChunkStore{}, embed: &fakeEmbedder{},
			opts: retrieval.Options{Dimensions: 4, MinSimilarity: 1.5},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := retrieval.NewService(tt.store, tt.embed, tt.opts); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestIndexStoresOneChunkPerPiece(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}
	note := aNote()

	if err := newService(t, store, embed).Index(context.Background(), note); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if len(store.replaced) != 1 {
		t.Fatalf("ReplaceChunks calls = %d, want 1", len(store.replaced))
	}

	chunks := store.replaced[0]
	if len(chunks) == 0 {
		t.Fatal("no chunks stored")
	}

	for i, chunk := range chunks {
		if chunk.NoteID != note.ID {
			t.Errorf("chunk %d has note id %s", i, chunk.NoteID)
		}

		if chunk.UserID != note.UserID {
			t.Errorf("chunk %d has user id %s", i, chunk.UserID)
		}

		if chunk.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, chunk.Ordinal)
		}

		if len(chunk.Embedding.Slice()) != testWidth {
			t.Errorf("chunk %d embedding width = %d, want %d", i, len(chunk.Embedding.Slice()), testWidth)
		}

		if chunk.CreatedAt.IsZero() {
			t.Errorf("chunk %d has no created timestamp", i)
		}
	}
}

// The title is the part a user searches on, so it has to be embedded even when
// the body never repeats it.
func TestIndexEmbedsTheTitleAndTags(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}

	if err := newService(t, store, embed).Index(context.Background(), aNote()); err != nil {
		t.Fatalf("Index: %v", err)
	}

	joined := strings.Join(embed.inputs, "\n")

	for _, want := range []string{"Rollback plan", "oncall", "release"} {
		if !strings.Contains(joined, want) {
			t.Errorf("embedded text is missing %q:\n%s", want, joined)
		}
	}
}

func TestIndexClearsChunksWhenTheNoteHasNoText(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}
	note := aNote()

	note.Title = "   "
	note.Content = ""
	note.Tags = domain.TagList("")

	if err := newService(t, store, embed).Index(context.Background(), note); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if len(store.replaced) != 0 {
		t.Error("stored chunks for a note with no text")
	}

	if len(store.deleted) != 1 || store.deleted[0] != note.ID {
		t.Errorf("deleted = %v, want the note's old chunks cleared", store.deleted)
	}
}

func TestIndexReplacesRatherThanAppends(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}
	service := newService(t, store, embed)
	note := aNote()

	if err := service.Index(context.Background(), note); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if err := service.Index(context.Background(), note); err != nil {
		t.Fatalf("Index again: %v", err)
	}

	if len(store.replaced) != 2 {
		t.Fatalf("ReplaceChunks calls = %d, want 2", len(store.replaced))
	}

	// Every replacement covers the same ordinals from zero, which is what makes it
	// a replacement rather than an append.
	for i, chunks := range store.replaced {
		for ordinal, chunk := range chunks {
			if chunk.Ordinal != ordinal {
				t.Errorf("call %d chunk %d has ordinal %d", i, ordinal, chunk.Ordinal)
			}
		}
	}
}

// A vector of the wrong width would fail much later as a constraint violation, so
// it is refused where it can be explained.
func TestIndexRejectsAWrongWidthEmbedding(t *testing.T) {
	store := &fakeChunkStore{}
	embed := &fakeEmbedder{vector: []float32{1, 2, 3}}

	err := newService(t, store, embed).Index(context.Background(), aNote())
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "3 dimensions") {
		t.Errorf("err = %v, want it to name the actual width", err)
	}

	if len(store.replaced) != 0 {
		t.Error("stored chunks despite the embedding being unusable")
	}
}

func TestIndexReportsAnEmbeddingFailure(t *testing.T) {
	store := &fakeChunkStore{}
	embed := &fakeEmbedder{err: errors.New("runtime unreachable")}

	if err := newService(t, store, embed).Index(context.Background(), aNote()); err == nil {
		t.Error("expected an error")
	}
}

func TestSearchEmbedsTheQueryAndReturnsHits(t *testing.T) {
	noteID, chunkID := uuid.New(), uuid.New()

	store := &fakeChunkStore{searchResult: []domain.NoteChunkMatch{
		{NoteChunk: domain.NoteChunk{ID: chunkID, NoteID: noteID, Ordinal: 2, Content: "rollback steps"}, Distance: 0.25},
	}}
	embed := &fakeEmbedder{}

	hits, err := newService(t, store, embed).Search(context.Background(), uuid.New(), "rollback", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}

	hit := hits[0]
	if hit.ChunkID != chunkID || hit.NoteID != noteID || hit.Ordinal != 2 {
		t.Errorf("hit = %+v", hit)
	}

	if hit.Similarity < 0.74 || hit.Similarity > 0.76 {
		t.Errorf("Similarity = %v, want 0.75 from distance 0.25", hit.Similarity)
	}

	if len(store.searched) != 1 {
		t.Errorf("search calls = %d, want 1", len(store.searched))
	}
}

// The repository filters on distance, so the similarity floor has to arrive there
// inverted.
func TestSearchConvertsTheSimilarityFloorToADistance(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}

	_, err := newService(t, store, embed).Search(context.Background(), uuid.New(), "anything", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if store.minDistance < 0.69 || store.minDistance > 0.71 {
		t.Errorf("min distance = %v, want 0.7 from a 0.3 similarity floor", store.minDistance)
	}

	if store.limit != 3 {
		t.Errorf("limit = %d, want the configured top k of 3", store.limit)
	}
}

func TestSearchUsesTheGivenTopK(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}

	if _, err := newService(t, store, embed).Search(context.Background(), uuid.New(), "q", 7); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if store.limit != 7 {
		t.Errorf("limit = %d, want 7", store.limit)
	}
}

// An empty query is not an error: the caller is usually building a prompt that
// may have nothing to ground it in.
func TestSearchReturnsNothingForABlankQuery(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}

	hits, err := newService(t, store, embed).Search(context.Background(), uuid.New(), "   ", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0", len(hits))
	}

	if len(store.searched) != 0 {
		t.Error("searched the database for a blank query")
	}

	if len(embed.inputs) != 0 {
		t.Error("embedded a blank query")
	}
}

func TestSearchPropagatesAStoreFailure(t *testing.T) {
	store := &fakeChunkStore{searchErr: errors.New("database down")}
	embed := &fakeEmbedder{}

	if _, err := newService(t, store, embed).Search(context.Background(), uuid.New(), "q", 0); err == nil {
		t.Error("expected an error")
	}
}

func TestUnindexRemovesTheNotesChunks(t *testing.T) {
	store, embed := &fakeChunkStore{}, &fakeEmbedder{}
	noteID := uuid.New()

	if err := newService(t, store, embed).Unindex(context.Background(), uuid.New(), noteID); err != nil {
		t.Fatalf("Unindex: %v", err)
	}

	if len(store.deleted) != 1 || store.deleted[0] != noteID {
		t.Errorf("deleted = %v, want the note's chunks", store.deleted)
	}
}

func TestIndexRejectsANilNote(t *testing.T) {
	if err := newService(t, &fakeChunkStore{}, &fakeEmbedder{}).Index(context.Background(), nil); err == nil {
		t.Error("expected an error")
	}
}
