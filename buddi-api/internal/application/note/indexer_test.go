package note_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// fakeIndexer records what it was asked to index and can be told to fail.
//
// Notes are snapshotted rather than stored by pointer: the service continues to
// mutate the same note after handing it over, so keeping the pointer would show the
// state the indexer finished with, not the state it was given.
type fakeIndexer struct {
	indexed []domain.Note
	err     error
}

func (f *fakeIndexer) Index(_ context.Context, n *domain.Note) error {
	f.indexed = append(f.indexed, *n)

	return f.err
}

func newIndexedService(t *testing.T, indexer note.Indexer) *note.Service {
	t.Helper()

	return note.NewService(newFakeNotes(), nil).WithIndexer(indexer)
}

func TestCreateIndexesTheStoredNote(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(indexer.indexed) != 1 {
		t.Fatalf("index calls = %d, want 1", len(indexer.indexed))
	}

	if indexer.indexed[0].ID != created.ID || indexer.indexed[0].UserID != userID {
		t.Errorf("indexed %+v, want the stored note", indexer.indexed[0])
	}
}

// The note is committed by the time indexing runs, so an embedding outage must
// not be reported as a lost note.
func TestCreateSucceedsWhenIndexingFails(t *testing.T) {
	service := newIndexedService(t, &fakeIndexer{err: errors.New("embedding runtime down")})

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create returned %v, want the note to survive an indexing failure", err)
	}

	if created.ID == uuid.Nil {
		t.Error("note was not created")
	}
}

func TestUpdateIndexesTheChangedNote(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := "The rollback steps now cover the read replica too."

	updated, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &content})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(indexer.indexed) != 2 {
		t.Fatalf("index calls = %d, want 2", len(indexer.indexed))
	}

	if indexer.indexed[1].Content != content {
		t.Errorf("indexed content = %q, want the updated text", indexer.indexed[1].Content)
	}

	if updated.ID != created.ID {
		t.Errorf("updated note id = %s, want %s", updated.ID, created.ID)
	}
}

func TestUpdateSucceedsWhenIndexingFails(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := "The rollback steps now cover the read replica too."

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &content}); err != nil {
		t.Fatalf("Update returned %v, want the note to survive an indexing failure", err)
	}
}

// A rejected update must not leave the index holding text the user never saved.
func TestUpdateDoesNotIndexARejectedChange(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	blank := "   "

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &blank}); err == nil {
		t.Fatal("expected the update to be rejected")
	}

	if len(indexer.indexed) != 1 {
		t.Errorf("index calls = %d, want 1: only the create", len(indexer.indexed))
	}
}

// Deleting a note cascades to its chunks, so the service must not also try to
// remove chunks it no longer owns.
func TestDeleteDoesNotIndex(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := service.Delete(context.Background(), userID, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(indexer.indexed) != 1 {
		t.Errorf("index calls = %d, want 1: only the create", len(indexer.indexed))
	}
}

// The indexer is optional, so the service must work without one.
func TestNotesWorkWithoutAnIndexer(t *testing.T) {
	service := note.NewService(newFakeNotes(), nil)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: "The rollback steps must stay under one page.",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := service.Get(context.Background(), created.UserID, created.ID); err != nil {
		t.Errorf("Get: %v", err)
	}
}
