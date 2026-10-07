package gormdb

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

func aChunk(t *testing.T, noteID uuid.UUID, userID uuid.UUID, ordinal int) domain.NoteChunk {
	t.Helper()

	return domain.NoteChunk{
		ID:         uuid.New(),
		NoteID:     noteID,
		UserID:     userID,
		Ordinal:    ordinal,
		Content:    "rollback steps",
		TokenCount: 3,
		Embedding:  pgvector.NewVector([]float32{0.1, 0.2, 0.3, 0.4}),
		CreatedAt:  time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
	}
}

func TestNoteChunksReplaceDeletesThenInserts(t *testing.T) {
	db, mock := newMockDatabase(t)
	noteID, userID := uuid.New(), uuid.New()
	chunks := []domain.NoteChunk{aChunk(t, noteID, userID, 0), aChunk(t, noteID, userID, 1)}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM "note_chunks"`)).
		WithArgs(noteID, userID).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "note_chunks"`)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	if err := NewNoteChunkRepository(db.Conn()).ReplaceChunks(context.Background(), chunks); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
}

// An empty batch cannot identify a note, so honouring it would silently leave the
// previous chunks searchable.
func TestNoteChunksReplaceRejectsAnEmptyBatch(t *testing.T) {
	db, _ := newMockDatabase(t)

	err := NewNoteChunkRepository(db.Conn()).ReplaceChunks(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}

// Deleting one note's chunks while inserting another's is a programming error that
// would otherwise read as a successful replace.
func TestNoteChunksReplaceRejectsAMixedBatch(t *testing.T) {
	db, _ := newMockDatabase(t)
	noteID, userID := uuid.New(), uuid.New()

	chunks := []domain.NoteChunk{
		aChunk(t, noteID, userID, 0),
		aChunk(t, uuid.New(), userID, 1),
	}

	if err := NewNoteChunkRepository(db.Conn()).ReplaceChunks(context.Background(), chunks); err == nil {
		t.Error("expected an error for chunks from two notes")
	}
}

func TestNoteChunksReplaceRollsBackOnInsertFailure(t *testing.T) {
	db, mock := newMockDatabase(t)
	noteID, userID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM "note_chunks"`)).
		WithArgs(noteID, userID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "note_chunks"`)).
		WillReturnError(sqlmock.ErrCancelled)
	mock.ExpectRollback()

	if err := NewNoteChunkRepository(db.Conn()).ReplaceChunks(
		context.Background(), []domain.NoteChunk{aChunk(t, noteID, userID, 0)},
	); err == nil {
		t.Error("expected the insert failure to surface")
	}
}

func TestNoteChunksListByNoteFiltersOwnerAndOrders(t *testing.T) {
	db, mock := newMockDatabase(t)
	userID, noteID := uuid.New(), uuid.New()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "note_chunks" WHERE user_id = $1 AND note_id = $2 ORDER BY ordinal ASC`)).
		WithArgs(userID, noteID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "note_id", "user_id", "ordinal"}).AddRow(
			uuid.New(), noteID, userID, 0))

	chunks, err := NewNoteChunkRepository(db.Conn()).ListByNote(context.Background(), userID, noteID)
	if err != nil {
		t.Fatalf("ListByNote: %v", err)
	}

	if len(chunks) != 1 || chunks[0].NoteID != noteID {
		t.Errorf("chunks = %+v", chunks)
	}
}

func TestNoteChunksDeleteByNoteFiltersOwner(t *testing.T) {
	db, mock := newMockDatabase(t)
	userID, noteID := uuid.New(), uuid.New()

	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM "note_chunks" WHERE user_id = $1 AND note_id = $2`)).
		WithArgs(userID, noteID).
		WillReturnResult(sqlmock.NewResult(0, 3))

	if err := NewNoteChunkRepository(db.Conn()).DeleteByNote(context.Background(), userID, noteID); err != nil {
		t.Fatalf("DeleteByNote: %v", err)
	}
}

// The tenant filter has to be part of the same statement as the vector ordering.
// Filtering afterwards would mean the index had already surfaced another user's
// chunks.
func TestNoteChunksSearchFiltersOwnerInTheSameStatement(t *testing.T) {
	db, mock := newMockDatabase(t)
	userID := uuid.New()
	query := pgvector.NewVector([]float32{0.1, 0.2, 0.3, 0.4})

	mock.ExpectQuery(`SELECT note_chunks\.\*, \(note_chunks\.embedding <=> \$1\) AS distance`).
		WithArgs(query, userID, query, 0.7, query, 5).
		WillReturnRows(sqlmock.NewRows([]string{"id", "note_id", "user_id", "ordinal", "content", "distance"}).
			AddRow(uuid.New(), uuid.New(), userID, 0, "rollback steps", 0.2))

	matches, err := NewNoteChunkRepository(db.Conn()).Search(context.Background(), userID, query, 5, 0.7)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}

	if matches[0].Distance != 0.2 {
		t.Errorf("Distance = %v, want 0.2", matches[0].Distance)
	}

	if matches[0].UserID != userID {
		t.Errorf("UserID = %v", matches[0].UserID)
	}
}

func TestNoteChunksSearchExcludesNullEmbeddings(t *testing.T) {
	db, mock := newMockDatabase(t)
	userID := uuid.New()
	query := pgvector.NewVector([]float32{0.1, 0.2, 0.3, 0.4})

	// The IS NOT NULL clause is what keeps an unindexed note out of the results.
	mock.ExpectQuery(`note_chunks\.embedding IS NOT NULL`).
		WithArgs(query, userID, query, 0.7, query, 5).
		WillReturnRows(sqlmock.NewRows([]string{"id", "distance"}))

	matches, err := NewNoteChunkRepository(db.Conn()).Search(context.Background(), userID, query, 5, 0.7)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) != 0 {
		t.Errorf("matches = %d, want 0", len(matches))
	}
}

func TestNoteChunksSearchSkipsTheQueryForAnUnusableLimit(t *testing.T) {
	db, _ := newMockDatabase(t)

	matches, err := NewNoteChunkRepository(db.Conn()).Search(
		context.Background(), uuid.New(), pgvector.NewVector([]float32{0.1}), 0, 0.7,
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(matches) != 0 {
		t.Errorf("matches = %d, want 0", len(matches))
	}
}
