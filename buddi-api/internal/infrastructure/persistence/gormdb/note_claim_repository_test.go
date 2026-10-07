package gormdb

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// The claim is one statement on purpose: a select followed by an update leaves a gap
// in which a second worker sees the same note as due, and two workers end up paying
// for the same embedding calls. SKIP LOCKED is what lets several workers drain the
// queue at once instead of queueing behind each other.
func TestNoteClaimLeasesRowsInOneStatement(t *testing.T) {
	db, mock := newMockDatabase(t)

	noteID, userID := uuid.New(), uuid.New()
	now := time.Date(2026, time.October, 5, 10, 0, 0, 0, time.UTC)
	leaseUntil := now.Add(2 * time.Minute)

	mock.ExpectQuery(`(?s)FOR UPDATE SKIP LOCKED.*UPDATE notes`).
		WithArgs(now, 5, 7, leaseUntil).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "user_id", "title", "content", "tags", "source", "archived_at",
			"search_state", "index_attempts", "last_index_error", "next_index_at",
			"indexed_at", "created_at", "updated_at",
		}).AddRow(
			noteID, userID, "Rollback plan", "the rollback steps", "{}", "manual", nil,
			domain.SearchStateIndexing, 0, nil, leaseUntil, nil, now, now,
		))

	claimed, err := NewNoteRepository(db.Conn()).ClaimDueNotesForIndexing(context.Background(), domain.IndexClaim{
		Now:         now,
		Limit:       7,
		Lease:       2 * time.Minute,
		MaxAttempts: 5,
	})
	if err != nil {
		t.Fatalf("ClaimDueNotesForIndexing: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed = %d notes, want 1", len(claimed))
	}

	if claimed[0].SearchState != domain.SearchStateIndexing {
		t.Errorf("claimed state = %q, want %q", claimed[0].SearchState, domain.SearchStateIndexing)
	}

	if !claimed[0].NextIndexAt.Equal(leaseUntil) {
		t.Errorf("claimed NextIndexAt = %v, want the lease end %v", claimed[0].NextIndexAt, leaseUntil)
	}

	if claimed[0].Content != "the rollback steps" {
		t.Errorf("claimed content = %q, want the note text to index", claimed[0].Content)
	}
}

// Every note is not what a caller asking for none intends, and the cheap way to be
// sure of that is to never issue the statement.
func TestNoteClaimOfNothingTouchesNoRows(t *testing.T) {
	db, mock := newMockDatabase(t)

	claimed, err := NewNoteRepository(db.Conn()).ClaimDueNotesForIndexing(context.Background(), domain.IndexClaim{
		Now:   time.Now(),
		Limit: 0,
		Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("ClaimDueNotesForIndexing: %v", err)
	}

	if len(claimed) != 0 {
		t.Errorf("claimed = %d notes, want 0", len(claimed))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected no statements: %v", err)
	}
}

// An edit that lands while a note is being embedded leaves the note the worker holds
// older than the row. Refusing the write is what stops a note being labelled
// searchable with chunks built from text that has been replaced.
func TestUpdateSearchStateReportsANoteThatMovedOn(t *testing.T) {
	db, mock := newMockDatabase(t)

	noteID, userID := uuid.New(), uuid.New()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE notes`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (SELECT 1 FROM notes WHERE`)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	err := NewNoteRepository(db.Conn()).UpdateSearchState(context.Background(), &domain.Note{
		ID:          noteID,
		UserID:      userID,
		SearchState: domain.SearchStateIndexed,
	})
	if !errors.Is(err, persistence.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
}

// Stale and missing are different answers. Reporting a note that still exists as
// missing invites a retry against a row that is fine, and reporting it as stale
// invites a repair that changes nothing.
func TestUpdateSearchStateReportsAMissingNote(t *testing.T) {
	db, mock := newMockDatabase(t)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE notes`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (SELECT 1 FROM notes WHERE`)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}))

	err := NewNoteRepository(db.Conn()).UpdateSearchState(context.Background(), &domain.Note{
		ID:     uuid.New(),
		UserID: uuid.New(),
	})
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Indexing is a side effect on the note's search state, so it must not look like an
// edit. GORM maintains updated_at on its own for Updates, which would rewrite the
// modification time of every note it indexes and break the guard above it.
func TestUpdateSearchStateDoesNotRestampTheNote(t *testing.T) {
	db, mock := newMockDatabase(t)

	edited := time.Date(2026, time.October, 5, 9, 0, 0, 0, time.UTC)
	noteID, userID := uuid.New(), uuid.New()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE notes`)).
		WithArgs(
			domain.SearchStateIndexed, 0, "", edited, nil, edited,
			noteID, userID, edited,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := NewNoteRepository(db.Conn()).UpdateSearchState(context.Background(), &domain.Note{
		ID:          noteID,
		UserID:      userID,
		SearchState: domain.SearchStateIndexed,
		NextIndexAt: edited,
		UpdatedAt:   edited,
	})
	if err != nil {
		t.Fatalf("UpdateSearchState: %v", err)
	}
}

// The note write and the search-state write have to agree about updated_at, because
// the second one refuses to run against a value the first one would have replaced
// with a timestamp of GORM's own.
func TestUpdateWritesTheNotesOwnUpdatedAt(t *testing.T) {
	db, mock := newMockDatabase(t)

	edited := time.Date(2026, time.October, 5, 9, 0, 0, 0, time.UTC)
	noteID, userID := uuid.New(), uuid.New()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE notes`)).
		WithArgs(
			"Rollback plan", "the rollback steps", domain.TagListOf([]string{"oncall"}), nil, edited,
			domain.SearchStatePending, 0, "", edited, nil,
			noteID, userID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := NewNoteRepository(db.Conn()).Update(context.Background(), &domain.Note{
		ID:          noteID,
		UserID:      userID,
		Title:       "Rollback plan",
		Content:     "the rollback steps",
		Tags:        domain.TagListOf([]string{"oncall"}),
		SearchState: domain.SearchStatePending,
		NextIndexAt: edited,
		UpdatedAt:   edited,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}
