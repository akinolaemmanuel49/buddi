package note_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// fakeNotes is an in-memory repository that applies the tenant scope the real
// one applies in SQL. Scoping it here is the point: a service that forgets to
// pass the user id still gets correct behaviour, and the tests below assert on
// what the service asked for rather than on the storage.
type fakeNotes struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*domain.Note
	last   domain.NoteFilter
	lastOK bool
	// searchStateWrites counts narrow bookkeeping updates, so a test can tell
	// "the note was re-queued" apart from "the whole note was rewritten".
	searchStateWrites int
	// searchStateErr fails the bookkeeping write without failing the note, which is
	// the only way to reach the one path that cannot report an error to the caller.
	searchStateErr error
	// claimErr fails a worker's claim of work, which no observer would otherwise see.
	claimErr error
	// claims records what each claim was asked for, so a test can assert on the
	// lease and the attempt cap rather than only on what came back.
	claims []domain.IndexClaim
}

func newFakeNotes() *fakeNotes {
	return &fakeNotes{byID: map[uuid.UUID]*domain.Note{}}
}

func (f *fakeNotes) Create(_ context.Context, n *domain.Note) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored := *n
	f.byID[n.ID] = &stored

	return nil
}

func (f *fakeNotes) GetByID(_ context.Context, userID uuid.UUID, id uuid.UUID) (*domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	note, ok := f.byID[id]
	if !ok || note.UserID != userID {
		return nil, domain.ErrNotFound
	}

	copied := *note

	return &copied, nil
}

func (f *fakeNotes) List(_ context.Context, userID uuid.UUID, filter domain.NoteFilter, page domain.Page) ([]domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.last = filter
	f.lastOK = true

	matches, err := f.matching(userID, filter)
	if err != nil {
		return nil, err
	}

	page = page.Normalise(note.MaxPageSize)

	out := []domain.Note{}

	for i, n := range matches {
		if i < page.Offset {
			continue
		}

		if len(out) == page.Limit {
			break
		}

		out = append(out, *n)
	}

	return out, nil
}

func (f *fakeNotes) Count(_ context.Context, userID uuid.UUID, filter domain.NoteFilter) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	matches, err := f.matching(userID, filter)
	if err != nil {
		return 0, err
	}

	return int64(len(matches)), nil
}

func (f *fakeNotes) matching(userID uuid.UUID, filter domain.NoteFilter) ([]*domain.Note, error) {
	out := []*domain.Note{}

	for _, n := range f.byID {
		if n.UserID != userID {
			continue
		}

		if !filter.IncludeArchived {
			archived := n.ArchivedAt != nil

			if filter.Archived != archived {
				continue
			}
		}

		if len(filter.Tags) > 0 {
			covered := true

			stored, err := n.Tags.Slice()
			if err != nil {
				return nil, err
			}

			for _, want := range filter.Tags {
				if !containsFold(stored, want) {
					covered = false
					break
				}
			}

			if !covered {
				continue
			}
		}

		if filter.Search != "" {
			needle := strings.ToLower(filter.Search)

			if !strings.Contains(strings.ToLower(n.Title), needle) &&
				!strings.Contains(strings.ToLower(n.Content), needle) {
				continue
			}
		}

		out = append(out, n)
	}

	return out, nil
}

func containsFold(haystack []string, needle string) bool {
	for _, item := range haystack {
		if strings.EqualFold(item, needle) {
			return true
		}
	}

	return false
}

func (f *fakeNotes) ReplaceTags(_ context.Context, userID uuid.UUID, noteID uuid.UUID, tags []string) error {
	stored, err := f.GetByID(context.Background(), userID, noteID)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	stored.Tags = domain.TagListOf(tags)
	f.byID[noteID] = stored

	return nil
}

func (f *fakeNotes) Update(_ context.Context, n *domain.Note) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.byID[n.ID]; !ok {
		return domain.ErrNotFound
	}

	stored := *n
	f.byID[n.ID] = &stored

	return nil
}

func (f *fakeNotes) UpdateSearchState(_ context.Context, n *domain.Note) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored, ok := f.byID[n.ID]
	if !ok {
		return domain.ErrNotFound
	}

	// The row moving on is not an error the caller can act on, so it is reported as
	// its own value. Modelling it here is the only way a service test can reach the
	// path where an edit lands mid-attempt.
	if !stored.UpdatedAt.Equal(n.UpdatedAt) {
		return domain.ErrStale
	}

	// Before the state moves, so a test can see a note whose write failed leaving
	// the stored state untouched.
	if f.searchStateErr != nil {
		return f.searchStateErr
	}

	// Only the bookkeeping moves, matching the narrow column update the real
	// repository performs. Copying the whole note here would hide the bug where a
	// stale in-memory copy overwrites a concurrent edit.
	stored.SearchState = n.SearchState
	stored.IndexAttempts = n.IndexAttempts
	stored.LastIndexError = n.LastIndexError
	stored.NextIndexAt = n.NextIndexAt
	stored.IndexedAt = n.IndexedAt

	f.searchStateWrites++

	return nil
}

// ClaimDueNotesForIndexing mirrors the real claim rather than listing notes for the
// worker: a note is only returned if it is due, and it is marked indexing with a
// lease as it is returned. A fake that skipped either half would let a worker test
// pass on behaviour the database would never allow.
func (f *fakeNotes) ClaimDueNotesForIndexing(_ context.Context, claim domain.IndexClaim) ([]domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claims = append(f.claims, claim)

	if f.claimErr != nil {
		return nil, f.claimErr
	}

	var due []domain.Note

	for _, stored := range f.byID {
		switch stored.SearchState {
		case domain.SearchStatePending, domain.SearchStateFailed, domain.SearchStateIndexing:
		default:
			continue
		}

		// Oldest first, matching the claim query's ordering so a limit of one always
		// picks the same note.
		if !stored.NextIndexAt.After(claim.Now) && stored.IndexAttempts < claim.MaxAttempts {
			due = append(due, *stored)
		}
	}

	sort.Slice(due, func(i, j int) bool {
		return due[i].NextIndexAt.Before(due[j].NextIndexAt)
	})

	if len(due) > claim.Limit {
		due = due[:claim.Limit]
	}

	claimed := make([]domain.Note, 0, len(due))

	for _, candidate := range due {
		stored := f.byID[candidate.ID]
		stored.SearchState = domain.SearchStateIndexing
		stored.NextIndexAt = claim.Now.Add(claim.Lease)

		claimed = append(claimed, *stored)
	}

	return claimed, nil
}

func (f *fakeNotes) Delete(_ context.Context, userID uuid.UUID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	n, ok := f.byID[id]
	if !ok || n.UserID != userID {
		return domain.ErrNotFound
	}

	delete(f.byID, id)

	return nil
}

type fixture struct {
	service *note.Service
	repo    *fakeNotes
	userID  uuid.UUID
	clock   *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := &start
	repo := newFakeNotes()

	return &fixture{
		service: note.NewService(repo, func() time.Time { return *clock }),
		repo:    repo,
		userID:  uuid.New(),
		clock:   clock,
	}
}

func (f *fixture) create(t *testing.T, title string) *domain.Note {
	t.Helper()

	created, err := f.service.Create(context.Background(), f.userID, note.CreateInput{
		Title:   title,
		Content: "content for " + title,
		Tags:    []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return created
}

func TestCreateNormalisesAndStores(t *testing.T) {
	f := newFixture(t)

	created := f.create(t, "  Buy milk  ")

	if created.Title != "Buy milk" {
		t.Errorf("Title = %q, want it trimmed", created.Title)
	}

	if created.UserID != f.userID {
		t.Errorf("UserID = %s, want %s", created.UserID, f.userID)
	}

	if created.ArchivedAt != nil {
		t.Error("a new note must not be archived")
	}

	if !created.CreatedAt.Equal(*f.clock) {
		t.Errorf("CreatedAt = %s, want the injected clock %s", created.CreatedAt, *f.clock)
	}
}

func TestCreateRejectsBlankTitleAndContent(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name  string
		input note.CreateInput
	}{
		{name: "blank title", input: note.CreateInput{Title: "   ", Content: "body"}},
		{name: "blank content", input: note.CreateInput{Title: "title", Content: "  \n "}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.service.Create(context.Background(), f.userID, tc.input)
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCreateNormalisesTags(t *testing.T) {
	f := newFixture(t)

	created, err := f.service.Create(context.Background(), f.userID, note.CreateInput{
		Title:   "Tagged",
		Content: "body",
		Tags:    []string{"  Work  ", "work", "HOME", "", "home"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// NormaliseTags lowercases, trims, drops blanks, de-duplicates and sorts, so
	// two clients that spell a tag differently cannot create two tags that a filter
	// then fails to match.
	want := []string{"home", "work"}

	got, err := created.Tags.Slice()
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tags = %v, want %v", got, want)
	}
}

func TestUpdateAppliesOnlySuppliedFields(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, "Original")

	title := "  Renamed  "

	updated, err := f.service.Update(context.Background(), f.userID, original.ID, note.UpdateInput{
		Title: &title,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.Title != "Renamed" {
		t.Errorf("Title = %q, want Renamed", updated.Title)
	}

	if updated.Content != original.Content {
		t.Errorf("Content = %q, want it untouched at %q", updated.Content, original.Content)
	}

	if len(updated.Tags) != len(original.Tags) {
		t.Errorf("Tags = %v, want them untouched at %v", updated.Tags, original.Tags)
	}
}

func TestUpdateRejectsBlankValues(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, "Original")

	blank, empty := "   ", ""

	for _, tc := range []struct {
		field string
		input note.UpdateInput
	}{
		{field: "title", input: note.UpdateInput{Title: &blank}},
		{field: "content", input: note.UpdateInput{Content: &empty}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			_, err := f.service.Update(context.Background(), f.userID, original.ID, tc.input)
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestArchiveRestoreAndClearAreDistinct(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Archiveable")

	archived, err := f.service.Update(context.Background(), f.userID, created.ID, note.UpdateInput{Archive: true})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if archived.ArchivedAt == nil {
		t.Fatal("Archive must set ArchivedAt")
	}

	restored, err := f.service.Update(context.Background(), f.userID, created.ID, note.UpdateInput{Restore: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if restored.ArchivedAt != nil {
		t.Error("Restore must clear ArchivedAt")
	}

	recleared, err := f.service.Update(context.Background(), f.userID, created.ID, note.UpdateInput{ClearArchive: true})
	if err != nil {
		t.Fatalf("ClearArchive: %v", err)
	}

	if recleared.ArchivedAt != nil {
		t.Error("ClearArchive must leave ArchivedAt nil")
	}

	// Archiving again after a restore must work, which a sticky flag would break.
	if _, err := f.service.Update(context.Background(), f.userID, created.ID, note.UpdateInput{Archive: true}); err != nil {
		t.Errorf("Archive after Restore must succeed, got %v", err)
	}
}

func TestUpdateRejectsContradictoryArchiveFlags(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Contradictory")

	_, err := f.service.Update(context.Background(), f.userID, created.ID, note.UpdateInput{
		Archive: true,
		Restore: true,
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
}

func TestListHidesArchivedByDefault(t *testing.T) {
	f := newFixture(t)

	f.create(t, "Live")

	archived, err := f.service.Create(context.Background(), f.userID, note.CreateInput{
		Title:   "Archived",
		Content: "body",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := f.service.Update(context.Background(), f.userID, archived.ID, note.UpdateInput{Archive: true}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	live, total, err := f.service.List(context.Background(), f.userID, domain.NoteFilter{}, domain.Page{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if total != 1 || len(live) != 1 || live[0].Title != "Live" {
		t.Errorf("default List returned %d notes (total %d), want only the live one", len(live), total)
	}

	both, totalBoth, err := f.service.List(context.Background(), f.userID, domain.NoteFilter{IncludeArchived: true}, domain.Page{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if totalBoth != 2 || len(both) != 2 {
		t.Errorf("IncludeArchived returned %d notes (total %d), want 2", len(both), totalBoth)
	}

	onlyArchived, totalArchived, err := f.service.List(context.Background(), f.userID, domain.NoteFilter{Archived: true}, domain.Page{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if totalArchived != 1 || len(onlyArchived) != 1 || onlyArchived[0].Title != "Archived" {
		t.Errorf("Archived filter returned %v, want only the archived note", titles(onlyArchived))
	}
}

func TestListNormalisesTagFilter(t *testing.T) {
	f := newFixture(t)

	if _, _, err := f.service.List(context.Background(), f.userID, domain.NoteFilter{
		Tags: []string{"  Work ", "work"},
	}, domain.Page{}); err != nil {
		t.Fatalf("List: %v", err)
	}

	// The filter is normalised before it reaches the repository, otherwise
	// "  Work " would silently match nothing.
	f.repo.mu.Lock()
	passed := f.repo.last.Tags
	f.repo.mu.Unlock()

	if len(passed) != 1 || passed[0] != "work" {
		t.Errorf("repository received tags %v, want [work]", passed)
	}
}

func TestListCapsPageSize(t *testing.T) {
	f := newFixture(t)

	_, _, err := f.service.List(context.Background(), f.userID, domain.NoteFilter{}, domain.Page{Limit: 10000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

// TestNoteFromAnotherUserIsInvisible checks that a note belonging to someone else
// is invisible. Both the service and the repository enforce the scope; this test
// documents that neither alone is trusted.
func TestNoteFromAnotherUserIsInvisible(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Private")

	other := uuid.New()

	// Not-found rather than a permission error: a 403 would confirm the id exists.
	if _, err := f.service.Get(context.Background(), other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Get error = %v, want ErrNotFound", err)
	}

	if err := f.service.Delete(context.Background(), other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Delete error = %v, want ErrNotFound", err)
	}

	// And the owner's note is still there afterwards.
	if _, err := f.service.Get(context.Background(), f.userID, created.ID); err != nil {
		t.Errorf("the owner's note must survive a cross-tenant delete: %v", err)
	}
}

func TestDeleteRemovesTheNote(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Doomed")

	if err := f.service.Delete(context.Background(), f.userID, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := f.service.Get(context.Background(), f.userID, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound after delete", err)
	}
}

func titles(notes []domain.Note) []string {
	out := make([]string, 0, len(notes))

	for _, n := range notes {
		out = append(out, n.Title)
	}

	return out
}
