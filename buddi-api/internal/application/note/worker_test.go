package note_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// fakeIndexerFunc adapts a function, so a test can make the indexer do something
// disruptive such as landing a concurrent edit.
type fakeIndexerFunc func(ctx context.Context, n *domain.Note) error

func (f fakeIndexerFunc) Index(ctx context.Context, n *domain.Note) error { return f(ctx, n) }

// aWorkerClock returns a fixed instant and a way to move it, because a worker's
// behaviour is mostly about time: leases, backoff and when a note becomes due.
type aWorkerClock struct{ now time.Time }

func (c *aWorkerClock) Now() time.Time { return c.now }

func (c *aWorkerClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newWorker(t *testing.T, repo *fakeNotes, indexer note.Indexer, clock *aWorkerClock, opts note.WorkerOptions) *note.Worker {
	t.Helper()

	opts.Clock = clock.Now

	worker, err := note.NewWorker(repo, indexer, opts)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	return worker
}

// queuedNote saves a note without an inline indexer, which is the state the worker
// exists to deal with: the text is stored and the note is waiting.
func queuedNote(t *testing.T, repo *fakeNotes, clock *aWorkerClock, userID uuid.UUID, title string) *domain.Note {
	t.Helper()

	created, err := note.NewService(repo, clock.Now).Create(context.Background(), userID, note.CreateInput{
		Title:   title,
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.SearchState != domain.SearchStatePending {
		t.Fatalf("queued note is %q, want %q", created.SearchState, domain.SearchStatePending)
	}

	return created
}

func newWorkerClock() *aWorkerClock {
	return &aWorkerClock{now: time.Date(2026, time.October, 5, 10, 0, 0, 0, time.UTC)}
}

func TestNewWorkerRequiresItsDependencies(t *testing.T) {
	if _, err := note.NewWorker(nil, &fakeIndexer{}, note.WorkerOptions{}); err == nil {
		t.Error("expected an error without a repository")
	}

	if _, err := note.NewWorker(newFakeNotes(), nil, note.WorkerOptions{}); err == nil {
		t.Error("expected an error without an indexer")
	}
}

func TestWorkerIndexesTheQueueAndEmptiesIt(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{}
	clock := newWorkerClock()
	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10})

	userID := uuid.New()
	first := queuedNote(t, repo, clock, userID, "Rollback plan")
	second := queuedNote(t, repo, clock, userID, "Deploy checklist")

	indexed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if indexed != 2 {
		t.Fatalf("indexed = %d, want 2", indexed)
	}

	if len(indexer.indexed) != 2 {
		t.Fatalf("index calls = %d, want 2", len(indexer.indexed))
	}

	for _, id := range []uuid.UUID{first.ID, second.ID} {
		stored, err := repo.GetByID(context.Background(), userID, id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}

		if !stored.Searchable() {
			t.Errorf("stored state = %q, want %q", stored.SearchState, domain.SearchStateIndexed)
		}

		if stored.IndexedAt == nil {
			t.Error("IndexedAt was not recorded")
		}
	}

	// The queue is empty now, which is the property the next pass depends on. A
	// worker that re-indexed the same notes forever would keep spending embedding
	// calls to rebuild identical chunks.
	indexed, err = worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	if indexed != 0 {
		t.Errorf("second pass indexed = %d, want 0", indexed)
	}
}

// The claim is what stops two workers doing the same job, and the lease is what
// stops one worker from being handed its own note back.
func TestWorkerHoldsItsClaimForTheLease(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{}
	clock := newWorkerClock()
	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{
		BatchSize: 10,
		Lease:     time.Minute,
	})

	queuedNote(t, repo, clock, uuid.New(), "Rollback plan")

	// Claimed by hand and deliberately left unfinished, as if the worker had died
	// mid-index.
	claimed, err := repo.ClaimDueNotesForIndexing(context.Background(), domain.IndexClaim{
		Now:         clock.Now(),
		Limit:       10,
		Lease:       time.Minute,
		MaxAttempts: 5,
	})
	if err != nil {
		t.Fatalf("ClaimDueNotesForIndexing: %v", err)
	}

	if len(claimed) != 1 || claimed[0].SearchState != domain.SearchStateIndexing {
		t.Fatalf("claimed = %#v, want one note marked %q", claimed, domain.SearchStateIndexing)
	}

	indexed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if indexed != 0 {
		t.Errorf("indexed = %d, want 0: the note is still leased", indexed)
	}

	if last := repo.claims[len(repo.claims)-1]; last.Lease != time.Minute {
		t.Errorf("worker claim = %#v, want the configured lease", last)
	}
}

// A worker that dies must not take its notes with it. The claim treats an expired
// lease as due, so recovery is the same query as normal work rather than a sweeper
// that has to agree about what "stuck" means.
func TestWorkerReclaimsAnExpiredLease(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{}
	clock := newWorkerClock()
	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10})

	queuedNote(t, repo, clock, uuid.New(), "Rollback plan")

	// A negative lease stands in for a worker that crashed: the claim is stamped
	// with a time that has already passed.
	if _, err := repo.ClaimDueNotesForIndexing(context.Background(), domain.IndexClaim{
		Now:         clock.Now(),
		Limit:       10,
		Lease:       -time.Minute,
		MaxAttempts: 5,
	}); err != nil {
		t.Fatalf("ClaimDueNotesForIndexing: %v", err)
	}

	indexed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if indexed != 1 {
		t.Errorf("indexed = %d, want 1: an expired lease is work waiting to be done", indexed)
	}
}

func TestWorkerRecordsAFailureAndSchedulesTheRetry(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	clock := newWorkerClock()
	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10})

	userID := uuid.New()
	queuedNote(t, repo, clock, userID, "Rollback plan")

	indexed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if indexed != 0 {
		t.Errorf("indexed = %d, want 0", indexed)
	}

	stored, err := repo.GetByID(context.Background(), userID, mustOnlyNote(t, repo).ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if stored.SearchState != domain.SearchStateFailed {
		t.Errorf("stored state = %q, want %q", stored.SearchState, domain.SearchStateFailed)
	}

	if stored.IndexAttempts != 1 {
		t.Errorf("IndexAttempts = %d, want 1", stored.IndexAttempts)
	}

	if stored.LastIndexError == "" {
		t.Error("the stored note does not say why it is unsearchable")
	}

	// Not retried on the next pass, or a runtime that is down costs a full batch of
	// embedding timeouts every interval.
	if !stored.NextIndexAt.After(clock.Now()) {
		t.Errorf("NextIndexAt = %v, want a retry scheduled after %v", stored.NextIndexAt, clock.Now())
	}

	before := len(indexer.indexed)

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	if len(indexer.indexed) != before {
		t.Error("the failed note was retried before its backoff elapsed")
	}

	clock.advance(2 * time.Minute)

	// The runtime comes back, which is the only reason the note is retried at all.
	indexer.err = nil

	if indexed, err = worker.RunOnce(context.Background()); err != nil || indexed != 1 {
		t.Errorf("after the backoff: indexed = %d, err = %v, want 1", indexed, err)
	}
}

// One broken dependency must not stop the notes behind it from being indexed.
func TestWorkerContinuesPastANoteThatFails(t *testing.T) {
	repo := newFakeNotes()
	clock := newWorkerClock()

	broken := queuedNote(t, repo, clock, uuid.New(), "Broken note")
	healthy := queuedNote(t, repo, clock, uuid.New(), "Healthy note")

	indexer := fakeIndexerFunc(func(_ context.Context, n *domain.Note) error {
		if n.ID == broken.ID {
			return errors.New("embedding runtime down")
		}

		return nil
	})

	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10})

	indexed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if indexed != 1 {
		t.Errorf("indexed = %d, want 1: the batch continues past the failure", indexed)
	}

	stored, err := repo.GetByID(context.Background(), healthy.UserID, healthy.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if !stored.Searchable() {
		t.Errorf("the note behind a failure is %q, want %q", stored.SearchState, domain.SearchStateIndexed)
	}
}

// A permanently broken embedder would otherwise be retried for the lifetime of the
// deployment, and every fix would arrive too late for the notes it would have saved.
func TestWorkerStopsRetryingANoteThatGaveUp(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{err: errors.New("embedding runtime down")}
	clock := newWorkerClock()
	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10, MaxAttempts: 2})

	queuedNote(t, repo, clock, uuid.New(), "Rollback plan")

	for range 4 {
		clock.advance(time.Hour)

		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	calls := len(indexer.indexed)
	if calls != 2 {
		t.Fatalf("index calls = %d, want 2", calls)
	}

	clock.advance(time.Hour)

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(indexer.indexed) != calls {
		t.Error("a note past the attempt cap was retried anyway")
	}

	if len(repo.claims) == 0 || repo.claims[len(repo.claims)-1].MaxAttempts != 2 {
		t.Errorf("claim = %#v, want the attempt cap passed to the repository", repo.claims)
	}
}

// An edit that lands while a note is being embedded leaves the stored note newer
// than the one the worker holds. Recording 'indexed' there would leave the note
// claiming to be searchable with chunks built from text that has been replaced, and
// nothing would re-queue it.
func TestWorkerDefersToANoteThatChangedWhileItWasEmbedding(t *testing.T) {
	repo := newFakeNotes()
	clock := newWorkerClock()
	observer := &recordingObserver{}

	userID := uuid.New()
	created := queuedNote(t, repo, clock, userID, "Rollback plan")

	indexer := fakeIndexerFunc(func(_ context.Context, claimed *domain.Note) error {
		// The concurrent edit: same note, new text, re-queued by the service.
		edited := *claimed
		edited.Content = "The rollback steps now cover the read replica too."
		edited.UpdatedAt = claimed.UpdatedAt.Add(time.Second)
		edited.MarkIndexPending(edited.UpdatedAt)

		if err := repo.Update(context.Background(), &edited); err != nil {
			return err
		}

		return nil
	})

	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10}).
		WithSearchStateObserver(observer)

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	stored, err := repo.GetByID(context.Background(), userID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if stored.SearchState != domain.SearchStatePending {
		t.Errorf("stored state = %q, want the edit's own %q", stored.SearchState, domain.SearchStatePending)
	}

	if stored.IndexedAt != nil {
		t.Error("a deferred attempt stamped an index time on a note it did not index")
	}

	// Not reported either: an edit landing mid-attempt is normal, not a fault.
	if len(observer.notes) != 0 {
		t.Errorf("reports = %d, want 0: a concurrent edit is not a failure", len(observer.notes))
	}

	// The stale chunks are replaced rather than trusted: the next pass indexes the
	// text that is actually stored. Past the edit's own due time, which is all it
	// takes for the note to be work again.
	clock.advance(time.Minute)

	recovered := newWorker(t, repo, &fakeIndexer{}, clock, note.WorkerOptions{BatchSize: 10})

	if indexed, err := recovered.RunOnce(context.Background()); err != nil || indexed != 1 {
		t.Fatalf("second RunOnce: indexed = %d, err = %v, want 1", indexed, err)
	}

	rebuilt, err := repo.GetByID(context.Background(), userID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if !rebuilt.Searchable() {
		t.Errorf("stored state = %q, want %q", rebuilt.SearchState, domain.SearchStateIndexed)
	}
}

func TestWorkerReportsAClaimFailure(t *testing.T) {
	repo := newFakeNotes()
	repo.claimErr = errors.New("connection reset")

	worker := newWorker(t, repo, &fakeIndexer{}, newWorkerClock(), note.WorkerOptions{BatchSize: 10})

	if _, err := worker.RunOnce(context.Background()); !errors.Is(err, repo.claimErr) {
		t.Fatalf("err = %v, want the repository failure", err)
	}
}

// The loop is the durable half of a best-effort capability, so it answers to
// cancellation and not to a failure it cannot fix.
func TestWorkerRunStopsWhenTheContextIsCancelled(t *testing.T) {
	repo := newFakeNotes()
	worker := newWorker(t, repo, &fakeIndexer{}, newWorkerClock(), note.WorkerOptions{
		BatchSize: 10,
		Interval:  time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		worker.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// Cancelling mid-batch must not block on the notes it has not reached yet, which is
// why the context is checked between notes rather than only between passes.
func TestWorkerRunOnceStopsBetweenNotesWhenCancelled(t *testing.T) {
	repo := newFakeNotes()
	clock := newWorkerClock()
	ctx, cancel := context.WithCancel(context.Background())

	queuedNote(t, repo, clock, uuid.New(), "First note")
	queuedNote(t, repo, clock, uuid.New(), "Second note")

	indexer := fakeIndexerFunc(func(_ context.Context, _ *domain.Note) error {
		cancel()
		return nil
	})

	worker := newWorker(t, repo, indexer, clock, note.WorkerOptions{BatchSize: 10})

	indexed, err := worker.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if indexed != 1 {
		t.Errorf("indexed = %d, want 1: the pass stops at the cancelled note", indexed)
	}
}

func TestWorkerUsesSensibleDefaultsForUnsetOptions(t *testing.T) {
	repo := newFakeNotes()
	indexer := &fakeIndexer{}

	// Unset options have to work together, so the clock is the real one here: the
	// defaults are the point rather than the injected time.
	queued, err := note.NewService(repo, nil).Create(context.Background(), uuid.New(), note.CreateInput{
		Title:   "Rollback plan",
		Content: aBody(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	worker, err := note.NewWorker(repo, indexer, note.WorkerOptions{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	if indexed, err := worker.RunOnce(context.Background()); err != nil || indexed != 1 {
		t.Fatalf("RunOnce: indexed = %d, err = %v, want 1", indexed, err)
	}

	stored, err := repo.GetByID(context.Background(), queued.UserID, queued.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if !stored.Searchable() {
		t.Errorf("stored state = %q, want %q", stored.SearchState, domain.SearchStateIndexed)
	}

	claim := repo.claims[0]
	if claim.Limit <= 0 || claim.Lease <= 0 || claim.MaxAttempts <= 0 {
		t.Errorf("claim = %#v, want positive batch, lease and attempt cap", claim)
	}
}

// mustOnlyNote returns the single note in a repository, so a test can read it back
// without repeating the lookup.
func mustOnlyNote(t *testing.T, repo *fakeNotes) domain.Note {
	t.Helper()

	if len(repo.byID) != 1 {
		t.Fatalf("notes = %d, want 1", len(repo.byID))
	}

	for _, stored := range repo.byID {
		return *stored
	}

	return domain.Note{}
}
