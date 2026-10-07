package note_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

func aBody() string {
	return "The rollback steps must stay under one page for the on-call engineer."
}

// The flag travels on the note row, so the queueing decision and the text are the
// same write. If these ever diverged, a note could be saved and never made
// searchable with nothing to show for it.
func TestCreateQueuesTheNoteForIndexing(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.SearchState != domain.SearchStateIndexed {
		t.Errorf("SearchState = %q, want %q after a successful index", created.SearchState, domain.SearchStateIndexed)
	}

	if created.IndexedAt == nil {
		t.Error("IndexedAt was not set after a successful index")
	}

	if created.IndexAttempts != 0 {
		t.Errorf("IndexAttempts = %d, want 0", created.IndexAttempts)
	}
}

func TestCreateRecordsAFailedIndex(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	service := newIndexedService(t, indexer)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.SearchState != domain.SearchStateFailed {
		t.Errorf("SearchState = %q, want %q", created.SearchState, domain.SearchStateFailed)
	}

	if created.IndexAttempts != 1 {
		t.Errorf("IndexAttempts = %d, want 1", created.IndexAttempts)
	}

	if created.LastIndexError == "" {
		t.Error("the reason is not searchable is not recorded")
	}

	// A retry is scheduled into the future rather than retried immediately, so a
	// runtime that is down is not hammered once per note.
	if !created.NextIndexAt.After(created.CreatedAt) {
		t.Errorf("NextIndexAt = %v, want a delay after %v", created.NextIndexAt, created.CreatedAt)
	}
}

func TestUpdateMarksTheNotePendingBeforeWriting(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A failing indexer leaves the note failed, so the next update has something to
	// recover from.
	indexer.err = errors.New("embedding runtime down")

	content := "The rollback steps now cover the read replica too."

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{
		Content: &content,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := indexer.indexed[len(indexer.indexed)-1].SearchState; got != domain.SearchStatePending {
		t.Errorf("the note handed to the indexer was %q, want %q so the chunk set is rebuilt", got, domain.SearchStatePending)
	}
}

// Re-embedding an unchanged note costs several embedding calls to rebuild an
// identical index, and archive is the most common PATCH by far.
func TestArchiveOnlyUpdateDoesNotReindex(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	before := len(indexer.indexed)

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Archive: true}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(indexer.indexed) != before {
		t.Errorf("index calls = %d, want %d: archiving does not change the text", len(indexer.indexed), before)
	}
}

func TestUpdateWithIdenticalTextDoesNotReindex(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
		Tags:    []string{"oncall"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	before := len(indexer.indexed)

	same := aBody()

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{
		Content: &same,
		Tags:    &[]string{"oncall"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(indexer.indexed) != before {
		t.Errorf("index calls = %d, want %d: nothing searchable changed", len(indexer.indexed), before)
	}
}

func TestUpdateWithNewTagsReindexes(t *testing.T) {
	indexer := &fakeIndexer{}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
		Tags:    []string{"oncall"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	before := len(indexer.indexed)

	if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{
		Tags: &[]string{"oncall", "release"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Tags are embedded with the note so they are searchable, which makes a tag
	// change a searchable change.
	if len(indexer.indexed) != before+1 {
		t.Errorf("index calls = %d, want %d", len(indexer.indexed), before+1)
	}
}

// The bookkeeping write is separate from the note write, so a test can tell that
// the state actually reached storage rather than only being set in memory.
func TestIndexStateIsPersistedNotJustSetInMemory(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	repo := newFakeNotes()
	service := note.NewService(repo, nil).WithIndexer(indexer)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if repo.searchStateWrites != 1 {
		t.Errorf("search state writes = %d, want 1", repo.searchStateWrites)
	}

	stored, err := repo.GetByID(context.Background(), created.UserID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if stored.SearchState != domain.SearchStateFailed {
		t.Errorf("stored SearchState = %q, want %q", stored.SearchState, domain.SearchStateFailed)
	}

	if stored.LastIndexError == "" {
		t.Error("the stored note does not say why it is unsearchable")
	}
}

// Without this the embedding runtime is paid on every edit to a note that will
// never index, turning one broken dependency into a slow API.
func TestANoteThatGaveUpIsNotRetriedInline(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	repo := newFakeNotes()
	service := note.NewService(repo, nil).WithIndexer(indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Exhaust the attempts as the service would. Each edit has to change the text:
	// an edit that changes nothing searchable is correctly not re-queued, so
	// reusing one string here would test nothing.
	for i := range 6 {
		content := aBody() + " edited " + strconv.Itoa(i)

		if _, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{
			Content: &content,
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	before := len(indexer.indexed)

	content := aBody() + " edited again"

	updated, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &content})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.IndexAttempts < 5 {
		t.Fatalf("IndexAttempts = %d, expected the note to have given up by now", updated.IndexAttempts)
	}

	if len(indexer.indexed) != before {
		t.Errorf("index calls = %d, want %d: a note that gave up is not retried inline",
			len(indexer.indexed), before)
	}
}

func TestARetriedEditResetsNothingAndKeepsTheFailureReason(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := aBody() + " edited"

	updated, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &content})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.LastIndexError == "" {
		t.Error("an edit cleared the reason the note is still unsearchable")
	}

	if updated.IndexedAt != nil {
		t.Error("a failed index left IndexedAt set from a previous success")
	}
}

func TestASuccessfulIndexClearsTheFailureReason(t *testing.T) {
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	service := newIndexedService(t, indexer)
	userID := uuid.New()

	created, err := service.Create(context.Background(), userID, note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	indexer.err = nil

	content := aBody() + " edited"

	updated, err := service.Update(context.Background(), userID, created.ID, note.UpdateInput{Content: &content})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.SearchState != domain.SearchStateIndexed {
		t.Errorf("SearchState = %q, want %q", updated.SearchState, domain.SearchStateIndexed)
	}

	if updated.LastIndexError != "" {
		t.Errorf("LastIndexError = %q, want it cleared once the note indexed", updated.LastIndexError)
	}
}

// TestRetryDelayGrowsAndIsCapped lives in retry_delay_internal_test.go, which is
// in-package: the delay is an unexported policy and exporting it to assert on it
// would widen the API for a test.

// recordingObserver captures what the service reports when it cannot store the
// outcome of an index attempt.
type recordingObserver struct {
	notes []domain.Note
	errs  []error
}

func (o *recordingObserver) ObserveSearchStateError(_ context.Context, n *domain.Note, err error) {
	o.notes = append(o.notes, *n)
	o.errs = append(o.errs, err)
}

// A failed bookkeeping write cannot be returned to the caller, because the note is
// already stored and the request is not allowed to fail over an index problem. That
// leaves it reportable in exactly one place, so the state this test covers is the
// difference between a broken index being visible and a database full of notes
// quietly disagreeing with the responses being served about them.
func TestAFailedStateWriteIsReportedRatherThanDropped(t *testing.T) {
	repo := newFakeNotes()
	repo.searchStateErr = errors.New("connection reset")

	observer := &recordingObserver{}
	service := note.NewService(repo, nil).
		WithIndexer(&fakeIndexer{err: errors.New("embedding runtime down")}).
		WithSearchStateObserver(observer)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(observer.notes) != 1 {
		t.Fatalf("reports = %d, want 1", len(observer.notes))
	}

	if !errors.Is(observer.errs[0], repo.searchStateErr) {
		t.Errorf("reported error = %v, want the repository failure", observer.errs[0])
	}

	// The note is in memory, but storage still holds the state from the note write,
	// so the report has to name the note that now disagrees with its stored row.
	if observer.notes[0].ID != created.ID {
		t.Errorf("reported note = %s, want %s", observer.notes[0].ID, created.ID)
	}

	stored, err := repo.GetByID(context.Background(), created.UserID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	// Storage still holds what the note write committed, which is the point: the
	// note the caller is holding claims to be failed and the row the next reader
	// loads claims to be pending. The report is what explains the difference.
	if stored.SearchState != domain.SearchStatePending {
		t.Errorf("stored SearchState = %q, want the queued %q the note write committed",
			stored.SearchState, domain.SearchStatePending)
	}

	if created.SearchState == stored.SearchState {
		t.Error("the returned note and the stored note agree, so this test is not exercising a lost write")
	}
}

// A successful index whose state write fails is the case that matters most: the chunks
// are in the database and are not searchable, because nothing recorded that.
func TestASuccessfulIndexWithAFailedStateWriteIsStillReported(t *testing.T) {
	repo := newFakeNotes()
	repo.searchStateErr = errors.New("connection reset")

	observer := &recordingObserver{}
	service := note.NewService(repo, nil).
		WithIndexer(&fakeIndexer{}).
		WithSearchStateObserver(observer)

	created, err := service.Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.SearchState != domain.SearchStateIndexed {
		t.Fatalf("SearchState = %q, want %q", created.SearchState, domain.SearchStateIndexed)
	}

	if len(observer.notes) != 1 {
		t.Fatalf("reports = %d, want 1", len(observer.notes))
	}

	if observer.notes[0].SearchState != domain.SearchStateIndexed {
		t.Errorf("reported state = %q, want %q", observer.notes[0].SearchState, domain.SearchStateIndexed)
	}
}
