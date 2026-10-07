package migrations

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
)

// TestSearchStateBackfillLabelsExistingNotes covers the one migration whose value is
// in the rows it rewrites rather than in the schema it creates.
//
// Everything else about migration 000004 is invisible to a test that migrates an empty
// database, which is exactly what the round trip test does: on an empty database the
// update matches nothing and reports success either way. The only way to catch a
// backfill that labels every row 'indexed', or stamps indexed_at on a note that has
// never been embedded, is to run it against a database that already has notes indexed
// the old way.
//
// Destructive, like the round trip test, and guarded the same way.
//
//	go test -run Backfill ./... with
//	BUDDI_TEST_DATABASE_URI=postgres://buddi:buddi@localhost:5432/buddi_migratest?sslmode=disable
func TestSearchStateBackfillLabelsExistingNotes(t *testing.T) {
	dsn := os.Getenv("BUDDI_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("set BUDDI_TEST_DATABASE_URI to a disposable database to run this test")
	}

	if databaseName(dsn) == "buddi" {
		t.Fatal("refusing to run destructive migration test against the primary database")
	}

	ctx := t.Context()

	if err := Down(ctx, dsn, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}

	backfillFile, backfill := migrationFile(t, "backfill")

	// Seeded at the version immediately before the backfill, so the rows it reads are
	// written without the help of any search_state column. Seeding after the migration
	// would test the column default instead of the update, which is the part that can
	// be wrong.
	migrateTo(t, dsn, backfill-1)

	pool := openTestPool(t, dsn)

	userID := insertTestUser(t, pool)

	// Three shapes of note, because the honest answer is not the same for all of them.
	// The middle one is the case a naive EXISTS(chunk) backfill gets wrong: the chunks
	// are there and the note is not searchable, because the missing vector is invisible
	// to the vector index.
	indexed := insertTestNote(t, pool, userID, "every chunk has a vector")
	partial := insertTestNote(t, pool, userID, "one chunk has no vector")
	unchunked := insertTestNote(t, pool, userID, "no chunks at all")

	first := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	last := first.Add(time.Minute)

	insertTestChunk(t, pool, indexed, userID, 0, true, first)
	insertTestChunk(t, pool, indexed, userID, 1, true, last)
	insertTestChunk(t, pool, partial, userID, 0, true, first)
	insertTestChunk(t, pool, partial, userID, 1, false, last)

	result, err := Up(ctx, dsn)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Checked against the applied list rather than the resulting version: Up runs
	// everything, and a migration added after the backfill would otherwise fail this
	// test for having nothing to do with it.
	if !slices.Contains(result.Applied, backfillFile) {
		t.Fatalf("Up applied %#v, want it to include %s", result.Applied, backfillFile)
	}

	if result.Version.Number < backfill {
		t.Fatalf("version after Up = %d, want at least the backfill migration %d",
			result.Version.Number, backfill)
	}

	for _, tc := range []struct {
		name string
		note uuid.UUID
		want string
		// wantIndexedAt is zero when the note must have no timestamp at all.
		wantIndexedAt time.Time
	}{
		{"fully embedded note is searchable", indexed, "indexed", last},
		{"note with an unembedded chunk is queued", partial, "pending", time.Time{}},
		{"note with no chunks is queued", unchunked, "pending", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, attempts, failure, next, indexedAt := readSearchState(t, pool, tc.note)

			if state != tc.want {
				t.Errorf("search_state = %q, want %q", state, tc.want)
			}

			// No attempt has been made against these rows, so any counter or message
			// here would be a claim about history that did not happen.
			if attempts != 0 {
				t.Errorf("index_attempts = %d, want 0", attempts)
			}

			if failure.Valid {
				t.Errorf("last_index_error = %q, want NULL", failure.String)
			}

			// NOT NULL with a default, but asserted because a NULL next_index_at makes
			// the row invisible to every claim query.
			if next.IsZero() {
				t.Error("next_index_at is NULL, so no claim query can ever match this note")
			}

			switch {
			case tc.wantIndexedAt.IsZero() && indexedAt.Valid:
				t.Errorf("indexed_at = %s, want NULL: a note that is not searchable has no index time", indexedAt.Time)
			case !tc.wantIndexedAt.IsZero() && !indexedAt.Valid:
				t.Errorf("indexed_at is NULL, want %s", tc.wantIndexedAt)
			case !tc.wantIndexedAt.IsZero() && !indexedAt.Time.Equal(tc.wantIndexedAt):
				t.Errorf("indexed_at = %s, want the newest chunk at %s", indexedAt.Time, tc.wantIndexedAt)
			}
		})
	}

	// The states above are only useful if a worker can find them, and the worker finds
	// them through a partial index. Asserting the index is partial is what proves the
	// backfilled rows are reachable by the intended query rather than merely by a
	// sequential scan, and asserting the predicate is what proves the index is partial
	// over the states this feature queues rather than some other subset.
	indexExists, indexValid, indexPartial, indexStates := indexState(t, pool, "notes_pending_index_idx")
	if !indexExists || !indexValid || !indexPartial {
		t.Errorf("notes_pending_index_idx exists=%v valid=%v partial=%v, want a valid partial index",
			indexExists, indexValid, indexPartial)
	}

	if want := []string{"pending", "failed", "indexing"}; !slices.Equal(indexStates, want) {
		t.Errorf("notes_pending_index_idx covers %v, want %v", indexStates, want)
	}

	// Leave the database migrated and empty, which is the state both this package's
	// tests and the running server expect.
	if err := Down(ctx, dsn, 0); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if _, err := Up(ctx, dsn); err != nil {
		t.Fatalf("final Up: %v", err)
	}
}

// migrateTo applies migrations up to a specific version, which the public Up cannot do
// because it always runs to the end. Needed to stage a database that has data but not
// the columns the backfill reads.
func migrateTo(t *testing.T, dsn string, version uint) {
	t.Helper()

	m, pool, err := newMigrator(dsn)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}

	defer closeMigrator(m, pool)

	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", version, err)
	}

	actual, err := currentVersion(t.Context(), pool)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}

	if actual.Number != version {
		t.Fatalf("staged version = %d, want %d", actual.Number, version)
	}
}

// migrationFile finds an up migration by a fragment of its name, so a test about the
// backfill does not have to be edited to survive a renumbering. It returns the file
// name too, for tests that care which migration ran rather than only how far the
// schema got.
func migrationFile(t *testing.T, fragment string) (string, uint) {
	t.Helper()

	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	for _, entry := range entries {
		version, ok := versionOf(entry.Name(), "up")
		if ok && strings.Contains(entry.Name(), fragment) {
			return entry.Name(), version
		}
	}

	t.Fatalf("no up migration matching %q is embedded", fragment)

	return "", 0
}

func insertTestUser(t *testing.T, pool *sql.DB) uuid.UUID {
	t.Helper()

	var id uuid.UUID

	err := pool.QueryRowContext(t.Context(),
		`INSERT INTO users (email, password_hash, display_name)
		 VALUES ('backfill@example.test', 'hash', 'Backfill')
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	return id
}

func insertTestNote(t *testing.T, pool *sql.DB, userID uuid.UUID, title string) uuid.UUID {
	t.Helper()

	var id uuid.UUID

	err := pool.QueryRowContext(t.Context(),
		`INSERT INTO notes (user_id, title, content) VALUES ($1, $2, 'body') RETURNING id`,
		userID, title,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert note %q: %v", title, err)
	}

	return id
}

// embeddingDimensions matches the note_chunks column and its check constraint, so a
// fixture that drifts from the schema fails loudly instead of testing something else.
const embeddingDimensions = 768

// insertTestChunk writes one chunk, optionally without a vector. A NULL embedding is
// legal in the schema and invisible to the vector index, which is the whole reason the
// backfill checks for it.
func insertTestChunk(
	t *testing.T,
	pool *sql.DB,
	noteID, userID uuid.UUID,
	ordinal int,
	embedded bool,
	createdAt time.Time,
) {
	t.Helper()

	const insert = `
		INSERT INTO note_chunks (note_id, user_id, ordinal, content, token_count, embedding, created_at)
		VALUES ($1, $2, $3, 'chunk body', 2, $4::vector, $5)`

	var embedding any

	if embedded {
		// Built rather than written out: an explicit 768-number literal in a test file
		// is unreadable, and a truncated copy of one fails the width check instead of
		// quietly testing a narrower column.
		embedding = "[" + strings.TrimSuffix(strings.Repeat("0,", embeddingDimensions), ",") + "]"
	}

	if _, err := pool.ExecContext(t.Context(), insert, noteID, userID, ordinal, embedding, createdAt); err != nil {
		t.Fatalf("insert chunk %d for note %s: %v", ordinal, noteID, err)
	}
}

func readSearchState(
	t *testing.T,
	pool *sql.DB,
	noteID uuid.UUID,
) (state string, attempts int, failure sql.NullString, next time.Time, indexedAt sql.NullTime) {
	t.Helper()

	const selectState = `
		SELECT search_state, index_attempts, last_index_error, next_index_at, indexed_at
		FROM notes
		WHERE id = $1`

	if err := pool.QueryRowContext(t.Context(), selectState, noteID).
		Scan(&state, &attempts, &failure, &next, &indexedAt); err != nil {
		t.Fatalf("read search state for note %s: %v", noteID, err)
	}

	return state, attempts, failure, next, indexedAt
}
