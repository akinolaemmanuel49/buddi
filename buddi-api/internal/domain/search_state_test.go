package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// A note that was never indexed and one that failed indexing look identical to a
// client that only sees "no results", which is the whole reason this is stored.
func TestSearchStateReportsWhetherANoteIsFindable(t *testing.T) {
	tests := map[domain.SearchState]bool{
		domain.SearchStatePending:  false,
		domain.SearchStateIndexing: false,
		domain.SearchStateFailed:   false,
		domain.SearchStateIndexed:  true,
		"":                         false,
	}

	for state, want := range tests {
		if got := (&domain.Note{ID: uuid.New(), SearchState: state}).Searchable(); got != want {
			t.Errorf("state %q: Searchable() = %v, want %v", state, got, want)
		}
	}
}

// A successful index clears the previous failure, or a note that was fixed would
// keep displaying a reason that no longer applies.
func TestMarkIndexedClearsTheFailureReason(t *testing.T) {
	now := time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC)

	n := &domain.Note{}
	n.MarkIndexFailed("embedding runtime down", 3, now)

	n.MarkIndexed(now.Add(time.Minute))

	if n.SearchState != domain.SearchStateIndexed {
		t.Errorf("SearchState = %q, want %q", n.SearchState, domain.SearchStateIndexed)
	}

	if n.LastIndexError != "" {
		t.Errorf("LastIndexError = %q, want it cleared", n.LastIndexError)
	}

	if n.IndexedAt == nil {
		t.Error("IndexedAt was not set")
	}
}

// Attempts are cumulative on purpose: a note that fails once, is edited, and fails
// again is a note with a problem, not a note that has had two unrelated failures.
func TestMarkIndexPendingKeepsTheRunningFailureCount(t *testing.T) {
	now := time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC)

	n := &domain.Note{}
	n.MarkIndexFailed("embedding runtime down", 3, now)
	n.MarkIndexPending(now)

	if n.IndexAttempts != 3 {
		t.Errorf("IndexAttempts = %d, want the count carried over", n.IndexAttempts)
	}

	if n.IndexedAt != nil {
		t.Error("an unindexed note kept an IndexedAt from a previous success")
	}

	if n.LastIndexError == "" {
		t.Error("re-queueing dropped the reason the note is not searchable")
	}
}

// The three grounding states are what let a caller tell "your notes do not cover
// this" from "search is broken". Collapsing them is what makes the feature
// untrustworthy, so the distinctions are pinned here.
func TestGroundingStatesAreDistinct(t *testing.T) {
	states := map[domain.GroundingState]struct{}{
		domain.GroundingGrounded:   {},
		domain.GroundingNoContext:  {},
		domain.GroundingUngrounded: {},
	}

	if len(states) != 3 {
		t.Errorf("distinct grounding states = %d, want 3", len(states))
	}

	if domain.GroundingNoContext == domain.GroundingUngrounded {
		t.Error("'searched and found nothing' must differ from 'could not search'")
	}
}
