// Package note implements the note use cases. It owns validation and the
// ordering of writes; persistence specifics stay in the repository.
package note

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// MaxPageSize bounds a single page of results.
const MaxPageSize = 100

// Indexer keeps a note's searchable chunks in step with its text.
//
// It is a port so the note service does not depend on which runtime produces the
// embeddings.
type Indexer interface {
	Index(ctx context.Context, note *domain.Note) error
}

// SearchStateObserver is notified when recording the outcome of an index attempt
// fails.
//
// It exists because refusing to fail a note write for an index problem also makes one
// failure invisible. The note is committed either way, so the only place the failure
// can be reported is here: a note whose chunks were written but whose state was not is
// a note the API will later describe as unsearchable, or as searchable when it is not,
// with nothing in the logs to explain it.
type SearchStateObserver interface {
	ObserveSearchStateError(ctx context.Context, note *domain.Note, err error)
}

// Service holds the dependencies of the note use cases.
type Service struct {
	bookkeeping indexBookkeeping
	indexer     Indexer
}

// NewService builds the service. now may be nil, in which case time.Now is used.
func NewService(notes domain.NoteRepository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}

	return &Service{bookkeeping: indexBookkeeping{notes: notes, now: now}}
}

// WithIndexer attaches a search indexer. Without one, notes are still fully
// usable and simply are not searchable.
func (s *Service) WithIndexer(indexer Indexer) *Service {
	s.indexer = indexer
	return s
}

// WithSearchStateObserver attaches a reporter for failed search-state writes. Without
// one the note service stays silent, which is the same as before the flags existed.
func (s *Service) WithSearchStateObserver(observer SearchStateObserver) *Service {
	s.bookkeeping.observer = observer
	return s
}

// reindex refreshes a note's chunks after its text has been stored, and records the
// outcome on the note itself.
//
// The note is committed by the time this runs, so a failure here cannot be reported
// as a failed write: the user's text is safe either way. What it does instead is
// leave the note marked failed, with the reason and a retry time, so a worker can
// pick it up later and a user asking "why isn't my note searchable" gets an answer.
//
// The index write itself is best effort for the same reason the note write is not
// transactional with it: holding a connection and a row lock across several
// sequential embedding calls would make saving a note depend on the embedding
// runtime being up.
func (s *Service) reindex(ctx context.Context, note *domain.Note) {
	if s.indexer == nil {
		return
	}

	// A note that has already given up is not retried inline. Without this, a
	// permanently broken embedding runtime makes every edit to that note pay the
	// full timeout again, which turns one bad dependency into a slow API for the
	// whole deployment. The worker skips those notes for the same reason.
	if note.IndexAttempts >= maxIndexAttempts {
		return
	}

	s.bookkeeping.finish(ctx, note, s.indexer.Index(ctx, note), s.bookkeeping.now())
}

// indexBookkeeping owns the note-side record of what an index attempt did.
//
// Shared by the inline path and the worker so both agree on what a failed attempt
// means: the same attempt counter, the same backoff, the same reporting. Split
// between them, the two would drift, and the first symptom would be a note that the
// inline path gave up on being retried forever by the worker.
type indexBookkeeping struct {
	notes    domain.NoteRepository
	observer SearchStateObserver
	now      func() time.Time
}

// finish records the outcome of one index attempt and persists it.
func (b indexBookkeeping) finish(ctx context.Context, note *domain.Note, indexErr error, now time.Time) {
	if indexErr != nil {
		attempt := note.IndexAttempts + 1
		note.MarkIndexFailed(indexErr.Error(), attempt, now.Add(indexRetryDelay(attempt)))
		b.record(ctx, note)

		return
	}

	note.MarkIndexed(now)
	b.record(ctx, note)
}

// record stores the outcome of an index attempt.
//
// Persisted separately from the chunks because the repository interface for chunks
// knows nothing about the note's own state. A failure here leaves the stored state
// pending, which is the correct state for "chunks written, status unknown": a re-index
// is harmless and the worker will get to it. It is reported rather than dropped,
// because the note in memory and the note in storage now disagree and only this
// process knows it.
func (b indexBookkeeping) record(ctx context.Context, note *domain.Note) {
	err := b.notes.UpdateSearchState(ctx, note)

	// A stale write means an edit landed while the attempt was running. The stored
	// row is the newer one and is already queued for indexing, so there is nothing
	// to fix and nothing to report: reporting it would turn a routine concurrent
	// edit into an error in the logs on every keystroke-sized edit.
	if errors.Is(err, domain.ErrStale) {
		return
	}

	if err != nil && b.observer != nil {
		b.observer.ObserveSearchStateError(ctx, note, err)
	}
}

// maxIndexAttempts is where a note stops being retried automatically. Past this it
// stays discoverable and marked failed, because a permanently broken embedder would
// otherwise be retried on every request for the lifetime of the deployment.
const maxIndexAttempts = 5

// indexRetryDelay grows the wait between attempts. The first retry is quick because
// the common cause is a model still loading; later ones back off because the cause is
// a runtime that is not coming back on its own.
func indexRetryDelay(attempt int) time.Duration {
	delay := time.Duration(1<<uint(attempt-1)) * time.Minute

	const ceiling = 30 * time.Minute

	if delay > ceiling {
		return ceiling
	}

	return delay
}

// CreateInput carries the fields of a new note.
type CreateInput struct {
	Title   string
	Content string
	Tags    []string
	Source  string
}

// Create validates and stores a new note.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, input CreateInput) (*domain.Note, error) {
	entity, err := domain.NewNote(userID, input.Title, input.Content, input.Tags, input.Source, s.bookkeeping.now())
	if err != nil {
		return nil, err
	}

	// Queued before the write, so it lands in the same statement. This is the whole
	// reason the flag lives on the note row: there is no window in which the text is
	// saved and the note is not queued to be made searchable.
	entity.MarkIndexPending(s.bookkeeping.now())

	if err := s.bookkeeping.notes.Create(ctx, entity); err != nil {
		return nil, err
	}

	s.reindex(ctx, entity)

	return entity, nil
}

// UpdateInput carries a partial update. A nil field means leave it alone, which
// is what makes PATCH different from PUT.
type UpdateInput struct {
	Title   *string
	Content *string
	Tags    *[]string
	// Archive, Restore and ClearArchive express the three states a note can be
	// asked to move to without a nullable boolean in the request body.
	Archive      bool
	Restore      bool
	ClearArchive bool
}

// Update applies a partial update to a note owned by userID.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, noteID uuid.UUID, input UpdateInput) (*domain.Note, error) {
	existing, err := s.bookkeeping.notes.GetByID(ctx, userID, noteID)
	if err != nil {
		return nil, err
	}

	now := s.bookkeeping.now()

	// Tracked rather than assumed, because the three inputs below are pointers and
	// most updates touch only the archive flag. Re-embedding a note whose text did
	// not change would cost several embedding calls to rebuild an identical index.
	textChanged := false

	if input.Title != nil {
		trimmed := strings.TrimSpace(*input.Title)
		if trimmed == "" {
			return nil, domain.Invalid("title is required", map[string]string{"title": "must not be empty"})
		}

		textChanged = textChanged || trimmed != existing.Title

		existing.Title = trimmed
	}

	if input.Content != nil {
		if strings.TrimSpace(*input.Content) == "" {
			return nil, domain.Invalid("content is required", map[string]string{"content": "must not be empty"})
		}

		textChanged = textChanged || *input.Content != existing.Content

		existing.Content = *input.Content
	}

	if input.Tags != nil {
		tags, err := domain.NormaliseTags(*input.Tags)
		if err != nil {
			return nil, err
		}

		currentTags, err := existing.Tags.Slice()
		if err != nil {
			// The stored value came from the database and was normalised on the way
			// in, so this only fires on data that never went through the service.
			// Comparing against an empty list would re-embed unnecessarily, which is
			// cheaper than rejecting an update over an internal inconsistency.
			currentTags = nil
		}

		textChanged = textChanged || !reflect.DeepEqual(tags, currentTags)

		existing.Tags = domain.TagListOf(tags)
	}

	// More than one archive instruction is contradictory rather than sequential,
	// so it is rejected instead of silently picking one.
	archiveRequests := 0

	for _, requested := range []bool{input.Archive, input.Restore, input.ClearArchive} {
		if requested {
			archiveRequests++
		}
	}

	if archiveRequests > 1 {
		return nil, domain.Invalid("conflicting archive instructions", map[string]string{
			"archived": "provide at most one of archive, restore or clear_archive",
		})
	}

	switch {
	case input.Archive:
		existing.Archive(now)
	case input.Restore:
		existing.Restore()
	case input.ClearArchive:
		existing.ArchivedAt = nil
	}

	existing.UpdatedAt = now

	// Only a change to searchable text needs re-embedding. Archiving or restoring a
	// note leaves the title, body and tags untouched, and re-embedding on those
	// would spend several embedding calls to produce an identical index.
	if textChanged {
		existing.MarkIndexPending(now)
	}

	if err := s.bookkeeping.notes.Update(ctx, existing); err != nil {
		return nil, err
	}

	if textChanged {
		s.reindex(ctx, existing)
	}

	return existing, nil
}

// List returns a page of the user's notes.
func (s *Service) List(ctx context.Context, userID uuid.UUID, filter domain.NoteFilter, page domain.Page) ([]domain.Note, int64, error) {
	normalised, err := normaliseFilter(filter)
	if err != nil {
		return nil, 0, err
	}

	page = page.Normalise(MaxPageSize)

	notes, err := s.bookkeeping.notes.List(ctx, userID, normalised, page)
	if err != nil {
		return nil, 0, err
	}

	total, err := s.bookkeeping.notes.Count(ctx, userID, normalised)
	if err != nil {
		return nil, 0, err
	}

	return notes, total, nil
}

// Get returns one note owned by userID.
func (s *Service) Get(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) (*domain.Note, error) {
	return s.bookkeeping.notes.GetByID(ctx, userID, noteID)
}

// Delete removes a note and, by cascade, its chunks.
func (s *Service) Delete(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) error {
	return s.bookkeeping.notes.Delete(ctx, userID, noteID)
}

func normaliseFilter(filter domain.NoteFilter) (domain.NoteFilter, error) {
	tags, err := domain.NormaliseTags(filter.Tags)
	if err != nil {
		return domain.NoteFilter{}, err
	}

	filter.Tags = tags

	return filter, nil
}
