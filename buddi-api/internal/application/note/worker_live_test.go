package note_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/gormdb"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence/migrations"
)

// The worker's unit tests use an in-memory repository, which cannot check the one
// thing that decides whether this feature works: whether the claim query is correct
// SQL. A claim that is subtly wrong still satisfies a fake that returns whatever the
// test asked for, and the failure only appears in production, as notes that are never
// indexed, or indexed twice, or indexed by two workers at once.
//
// So these run against a real PostgreSQL with a stub indexer. What is under test is
// the queue; the embeddings are not.
//
//	go test ./internal/application/note with
//	BUDDI_TEST_DATABASE_URI=postgres://buddi:buddi@localhost:5432/buddi_test?sslmode=disable
func TestLiveWorkerIndexesAQueuedNoteOnce(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Deploy plan", "the deploy steps", "oncall")

	var mu sync.Mutex

	var indexed []uuid.UUID

	worker := liveWorker(t, repo, time.Now, func(_ context.Context, n *domain.Note) error {
		mu.Lock()
		defer mu.Unlock()

		indexed = append(indexed, n.ID)

		return nil
	})

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(indexed) != 1 || indexed[0] != saved.ID {
		t.Fatalf("indexed %v, want just the queued note %s", indexed, saved.ID)
	}

	stored := liveNoteState(t, repo, user.ID, saved.ID)

	if stored.SearchState != domain.SearchStateIndexed {
		t.Errorf("search_state = %q, want %q", stored.SearchState, domain.SearchStateIndexed)
	}

	if stored.IndexedAt == nil {
		t.Error("indexed_at is nil, want the time the note finished indexing")
	}

	// A second pass has nothing to do. This is the check that the write on the way out
	// did not re-queue the note it had just finished indexing.
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	if len(indexed) != 1 {
		t.Errorf("indexed %v after a second pass, want the note indexed only once", indexed)
	}
}

// Indexing must not look like an edit. The note's modification time is the guard
// against a stale index write landing after an edit, so the whole design rests on the
// bookkeeping write leaving updated_at alone.
//
// Compared against a read of the row rather than against the struct the service built,
// because that struct never came back from the database: PostgreSQL stores microseconds
// and the rest of a nanosecond timestamp is dropped on the way in.
func TestLiveWorkerLeavesTheNotesTimestampAlone(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Rollback plan", "the rollback steps", "oncall")
	before := liveNoteState(t, repo, user.ID, saved.ID)

	worker := liveWorker(t, repo, time.Now, func(context.Context, *domain.Note) error { return nil })

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	stored := liveNoteState(t, repo, user.ID, saved.ID)

	if !stored.UpdatedAt.UTC().Equal(before.UpdatedAt.UTC()) {
		t.Errorf("updated_at = %v, want it unchanged at %v: indexing is not an edit",
			stored.UpdatedAt.UTC(), before.UpdatedAt.UTC())
	}
}

// The guard above compares an in-memory timestamp against the stored row, so the two
// have to be the same value or every state write looks like a concurrent edit and the
// inline path never reaches 'indexed'.
func TestLiveInlineIndexingReachesIndexed(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	// The service's own clock, with no worker: this is the path a request takes when
	// the embedding runtime is up.
	service := note.NewService(repo, nil).WithIndexer(liveIndexer(func(context.Context, *domain.Note) error { return nil }))

	saved, err := service.Create(ctx, user.ID, note.CreateInput{
		Title:   "Runbook",
		Content: "how to restart the api",
		Tags:    []string{"oncall"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stored := liveNoteState(t, repo, user.ID, saved.ID)

	if stored.SearchState != domain.SearchStateIndexed {
		t.Errorf("search_state = %q, want %q", stored.SearchState, domain.SearchStateIndexed)
	}

	if stored.IndexedAt == nil {
		t.Error("indexed_at is nil, want the time the note finished indexing")
	}
}

func TestLiveWorkerHoldsAClaimUntilTheLeaseExpires(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Escalation path", "who to wake up", "oncall")

	now := time.Now().UTC()

	claim := func(at time.Time) []domain.Note {
		t.Helper()

		claims, err := repo.ClaimDueNotesForIndexing(ctx, domain.IndexClaim{
			Now: at, Limit: 10, Lease: time.Minute, MaxAttempts: 5,
		})
		if err != nil {
			t.Fatalf("claim at %v: %v", at, err)
		}

		return claims
	}

	if got := claim(now); len(got) != 1 || got[0].ID != saved.ID {
		t.Fatalf("first claim = %v, want just %s", got, saved.ID)
	}

	// The claim moved the note to 'indexing' with a lease a minute out, which is what
	// keeps a second worker off it while the first embeds.
	held := liveNoteState(t, repo, user.ID, saved.ID)
	if held.SearchState != domain.SearchStateIndexing {
		t.Fatalf("search_state = %q, want %q", held.SearchState, domain.SearchStateIndexing)
	}

	if got := claim(now.Add(30 * time.Second)); len(got) != 0 {
		t.Errorf("claimed %v while the lease was still held", got)
	}

	// Once the lease has run out the note is claimable again, which is what makes a
	// crashed worker's notes recoverable instead of stuck in 'indexing' for good.
	if got := claim(now.Add(2 * time.Minute)); len(got) != 1 || got[0].ID != saved.ID {
		t.Errorf("claim after the lease expired = %v, want %s", got, saved.ID)
	}
}

func TestLiveWorkerDoesNotStealALeasedNote(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Escalation path", "who to wake up", "oncall")

	clock := func() time.Time { return time.Now().UTC() }

	var (
		mu    sync.Mutex
		stole int
	)

	// A second worker over the same database. This is the concurrency the claim exists
	// for: a claim that did not lock would hand one note to both of them, and both
	// would embed it.
	other, err := note.NewWorker(repo, liveIndexer(func(context.Context, *domain.Note) error {
		mu.Lock()
		defer mu.Unlock()

		stole++

		return nil
	}), note.WorkerOptions{Clock: clock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	first, err := note.NewWorker(repo, liveIndexer(func(_ context.Context, n *domain.Note) error {
		if n.ID != saved.ID {
			t.Errorf("claimed %s, want %s", n.ID, saved.ID)
		}

		// While the first worker holds the note, the second must find nothing at all.
		if _, err := other.RunOnce(ctx); err != nil {
			t.Errorf("second worker RunOnce: %v", err)
		}

		return nil
	}), note.WorkerOptions{Clock: clock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	if _, err := first.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	mu.Lock()
	held := stole
	mu.Unlock()

	if held != 0 {
		t.Errorf("the second worker indexed %d notes that were still leased", held)
	}
}

func TestLiveWorkerRetriesAndThenGivesUp(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Outage notes", "what broke", "oncall")

	now := time.Now().UTC()
	clock := func() time.Time { return now }

	var attempts int

	worker, err := note.NewWorker(repo, liveIndexer(func(context.Context, *domain.Note) error {
		attempts++

		return errors.New("embedding runtime unreachable")
	}), note.WorkerOptions{Clock: clock, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	// Three passes, moving past the backoff in between. The note is claimed again each
	// time because the failure rescheduled it rather than dropping it.
	for pass := range 3 {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}

		stored := liveNoteState(t, repo, user.ID, saved.ID)

		if stored.IndexAttempts != pass+1 {
			t.Fatalf("index_attempts after pass %d = %d, want %d", pass, stored.IndexAttempts, pass+1)
		}

		if stored.NextIndexAt.IsZero() {
			t.Fatalf("next_index_at after pass %d is nil, want a retry time", pass)
		}

		// Not yet due: the backoff is what stops an unreachable runtime from being
		// hammered once a second.
		claims, err := repo.ClaimDueNotesForIndexing(ctx, domain.IndexClaim{
			Now: now, Limit: 10, Lease: time.Minute, MaxAttempts: 5,
		})
		if err != nil {
			t.Fatalf("claim before the backoff: %v", err)
		}

		if len(claims) != 0 {
			t.Errorf("claimed %d notes before their retry time", len(claims))
		}

		now = now.Add(31 * time.Minute)
	}

	if attempts != 3 {
		t.Errorf("the indexer was called %d times, want 3", attempts)
	}

	stored := liveNoteState(t, repo, user.ID, saved.ID)

	if stored.SearchState != domain.SearchStateFailed {
		t.Errorf("search_state = %q, want %q", stored.SearchState, domain.SearchStateFailed)
	}

	if !strings.Contains(stored.LastIndexError, "embedding runtime unreachable") {
		t.Errorf("last_index_error = %q, want the failure that caused it", stored.LastIndexError)
	}

	if stored.IndexedAt != nil {
		t.Errorf("indexed_at = %v, want nil for a note that never indexed", stored.IndexedAt)
	}

	// The attempt cap is what turns an unfixable note into a stopped one instead of a
	// permanent background cost.
	now = now.Add(31 * time.Minute)

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce after the cap: %v", err)
	}

	if attempts != 3 {
		t.Errorf("the indexer was called %d times after the cap, want it left at 3", attempts)
	}
}

func TestLiveWorkerLosesToAConcurrentEdit(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)
	user := liveUser(t, db)

	saved := liveNote(t, repo, user.ID, "Release notes", "what shipped", "oncall")

	revised := "what shipped, revised after a bad release"

	// The edit lands while the index attempt is running, which is the only order in
	// which the two are ever out of step: the worker read the note before it began.
	//
	// It goes through the service rather than straight to the repository, because that
	// is what a request does and because queueing is the service's job: the newer text
	// has to re-queue itself or there is nothing left to recover.
	//
	// Its own indexing fails, so the newer version is left visibly unfinished. If the
	// older attempt could write over that, the note would end up claiming to be
	// searchable with chunks built from text the user has already replaced.
	edits := note.NewService(repo, nil).WithIndexer(liveIndexer(func(context.Context, *domain.Note) error {
		return errors.New("embedding runtime unreachable")
	}))

	worker := liveWorker(t, repo, time.Now, func(_ context.Context, claimed *domain.Note) error {
		_, err := edits.Update(ctx, claimed.UserID, claimed.ID, note.UpdateInput{Content: &revised})

		return err
	})

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	stored := liveNoteState(t, repo, user.ID, saved.ID)

	// The newer version's outcome is what has to survive. An older attempt that wins
	// this race leaves the note claiming to be searchable with chunks built from text
	// the user has already replaced, and nothing re-queues it, because the state says
	// it is done.
	if stored.SearchState != domain.SearchStateFailed {
		t.Errorf("search_state = %q, want %q from the newer edit's own failure",
			stored.SearchState, domain.SearchStateFailed)
	}

	if stored.Content != revised {
		t.Errorf("content = %q, want the edited text", stored.Content)
	}

	if stored.IndexedAt != nil {
		t.Errorf("indexed_at = %v, want nil after losing the race", stored.IndexedAt)
	}

	if stored.IndexAttempts != 1 {
		t.Errorf("index_attempts = %d, want 1 from the newer edit alone", stored.IndexAttempts)
	}

	// And the next pass picks it up, which is what makes losing recoverable.
	later := time.Now().UTC().Add(time.Hour)

	var retried []string

	if _, err := liveWorker(t, repo, func() time.Time { return later },
		func(_ context.Context, n *domain.Note) error {
			retried = append(retried, n.Content)

			return nil
		}).RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	if len(retried) != 1 || retried[0] != revised {
		t.Errorf("second pass claimed %q, want the edited note", retried)
	}
}

func TestLiveWorkerKeepsOneUsersNotesOutOfAnotherUsersHands(t *testing.T) {
	ctx := t.Context()
	db, repo := liveDatabase(t)

	mine := liveUser(t, db)
	theirs := liveUser(t, db)

	saved := liveNote(t, repo, mine.ID, "Private plan", "not for sharing", "oncall")

	// The queue is global: one worker drains every user's notes, because the embedder
	// and its budget are shared. What must not follow from that is a note becoming
	// readable by somebody else, so the claim is checked for who owns it.
	worker := liveWorker(t, repo, time.Now, func(_ context.Context, claimed *domain.Note) error {
		if claimed.UserID != mine.ID {
			t.Errorf("claimed a note owned by %s", claimed.UserID)
		}

		return nil
	})

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, err := repo.GetByID(ctx, theirs.ID, saved.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("another user can read the note: err = %v, want ErrNotFound", err)
	}
}

// liveDatabase returns a database of this test's own, derived from
// BUDDI_TEST_DATABASE_URI. Its own, because the migration tests drop every table in the
// database they are given and Go runs test packages in parallel: sharing one would make
// this file fail whenever it was not the slower of the two.
func liveDatabase(t *testing.T) (*gorm.DB, domain.NoteRepository) {
	t.Helper()

	dsn := liveTestDSN(t)

	if _, err := migrations.Up(t.Context(), dsn); err != nil {
		t.Fatalf("migrate %s: %v", dsn, err)
	}

	db := gormdb.New(gormdb.DefaultConfig(dsn))
	if err := db.Connect(t.Context()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	conn := db.Conn()

	// Every test in this file shares one database, and the queue is global: a note left
	// queued by one test is claimed by the next one and counted against its assertions.
	// Users can stay, because notes cascade away with their chunks.
	if err := conn.Exec("DELETE FROM notes").Error; err != nil {
		t.Fatalf("clear notes: %v", err)
	}

	return conn, gormdb.NewNoteRepository(conn)
}

func liveTestDSN(t *testing.T) string {
	t.Helper()

	configured := os.Getenv("BUDDI_TEST_DATABASE_URI")
	if configured == "" {
		t.Skip("set BUDDI_TEST_DATABASE_URI to a disposable database to run this test")
	}

	parsed, err := url.Parse(configured)
	if err != nil {
		t.Fatalf("parse BUDDI_TEST_DATABASE_URI: %v", err)
	}

	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" || name == "buddi" {
		t.Fatalf("refusing to run against %q, which is not a disposable database", name)
	}

	parsed.Path = "/" + strings.TrimSuffix(name, "_noteindex") + "_noteindex"

	liveCreateDatabase(t, configured, strings.TrimPrefix(parsed.Path, "/"))

	return parsed.String()
}

// liveCreateDatabase creates the test database if it is missing, through the
// maintenance database, because CREATE DATABASE cannot run against the database it is
// creating.
func liveCreateDatabase(t *testing.T, configured, name string) {
	t.Helper()

	maintenance, err := url.Parse(configured)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}

	maintenance.Path = "/postgres"

	pool, err := sql.Open("pgx", maintenance.String())
	if err != nil {
		t.Fatalf("open maintenance database: %v", err)
	}

	defer func() { _ = pool.Close() }()

	// "already exists" is the expected outcome on every run after the first and is not
	// worth reporting.
	if _, err := pool.ExecContext(t.Context(), fmt.Sprintf("CREATE DATABASE %s", name)); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create %s: %v", name, err)
	}
}

func liveUser(t *testing.T, db *gorm.DB) *domain.User {
	t.Helper()

	user, err := domain.NewUser(fmt.Sprintf("%s@example.com", uuid.NewString()), "not-a-real-hash", "Test", time.Now())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}

	if err := db.Create(user).Error; err != nil {
		t.Fatalf("insert user: %v", err)
	}

	return user
}

func liveNote(t *testing.T, repo domain.NoteRepository, userID uuid.UUID, title, content, tag string) *domain.Note {
	t.Helper()

	saved, err := note.NewService(repo, nil).Create(t.Context(), userID, note.CreateInput{
		Title:   title,
		Content: content,
		Tags:    []string{tag},
	})
	if err != nil {
		t.Fatalf("create note: %v", err)
	}

	return saved
}

func liveNoteState(t *testing.T, repo domain.NoteRepository, userID, id uuid.UUID) *domain.Note {
	t.Helper()

	stored, err := repo.GetByID(t.Context(), userID, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	return stored
}

func liveWorker(t *testing.T, repo domain.NoteRepository, clock func() time.Time, index liveIndexer) *note.Worker {
	t.Helper()

	worker, err := note.NewWorker(repo, index, note.WorkerOptions{Clock: clock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	return worker
}

// liveIndexer adapts a function to the indexer port.
type liveIndexer func(context.Context, *domain.Note) error

func (f liveIndexer) Index(ctx context.Context, n *domain.Note) error { return f(ctx, n) }
