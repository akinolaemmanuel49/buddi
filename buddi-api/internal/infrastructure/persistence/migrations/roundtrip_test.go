package migrations

import (
	"database/sql"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestUpDownRoundTrip is destructive: it drops every object the migrations
// create. It only runs when BUDDI_TEST_DATABASE_URI points at a throwaway
// database, and refuses to touch anything whose name looks like a real one.
//
//	go test -run RoundTrip ./... with
//	BUDDI_TEST_DATABASE_URI=postgres://buddi:buddi@localhost:5432/buddi_migratest?sslmode=disable
func TestUpDownRoundTrip(t *testing.T) {
	dsn := os.Getenv("BUDDI_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("set BUDDI_TEST_DATABASE_URI to a disposable database to run this test")
	}

	if databaseName(dsn) == "buddi" {
		t.Fatal("refusing to run destructive migration test against the primary database")
	}

	ctx := t.Context()

	// Reset first so the test is repeatable. It only ever runs against a throwaway
	// database, and without this it passes once and then reports "applied no
	// migrations" on every rerun because the previous run left it migrated.
	if err := Down(ctx, dsn, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}

	result, err := Up(ctx, dsn)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(result.Applied) == 0 {
		t.Fatal("Up applied no migrations")
	}

	// Derived from the embedded set rather than written down, because a hardcoded
	// number silently becomes a lie the first time a migration is added and fails
	// for a reason that has nothing to do with the code under test.
	latest := highestEmbeddedVersion(t)

	if result.Version.Number != latest {
		t.Fatalf("version = %d, want the highest embedded version %d", result.Version.Number, latest)
	}

	pool := openTestPool(t, dsn)

	for _, table := range []string{"users", "refresh_tokens", "notes", "note_chunks", "tasks", "agent_runs", "approvals"} {
		if !tableExists(t, pool, table) {
			t.Errorf("table %s was not created", table)
		}
	}

	// The search flags are the reason migration 000003 exists, so their absence is
	// the specific failure worth catching here rather than at the first note write.
	for _, column := range []struct{ table, name string }{
		{"notes", "search_state"},
		{"notes", "index_attempts"},
		{"notes", "last_index_error"},
		{"notes", "next_index_at"},
		{"notes", "indexed_at"},
		{"agent_runs", "grounding_state"},
	} {
		if !columnExists(t, pool, column.table, column.name) {
			t.Errorf("column %s.%s was not created", column.table, column.name)
		}
	}

	// The claim query the indexing worker will run reads notes through a partial
	// index. Asserting the index exists here catches a down migration that dropped it
	// or an up migration that forgot it, which is otherwise invisible: without the
	// index the queries still work, just slowly, and the table scan only shows up
	// when the table is big enough to matter. The predicate is asserted too, because
	// the states it covers decide whether a worker that died mid-note is ever noticed.
	exists, valid, partial, states := indexState(t, pool, "notes_pending_index_idx")
	if !exists || !valid || !partial {
		t.Errorf("notes_pending_index_idx exists=%v valid=%v partial=%v, want a valid partial index", exists, valid, partial)
	}

	if want := []string{"pending", "failed", "indexing"}; !slices.Equal(states, want) {
		t.Errorf("notes_pending_index_idx covers %v, want %v", states, want)
	}

	// Applying again must be a no-op rather than an error.
	second, err := Up(ctx, dsn)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}

	if len(second.Applied) != 0 {
		t.Errorf("second Up applied %#v, want nothing", second.Applied)
	}

	// Down by zero steps means every migration, so this asserts the full teardown
	// and not just the last one. Naming a step count here would silently stop
	// testing the earlier down migrations every time another is added.
	if err := Down(ctx, dsn, 0); err != nil {
		t.Fatalf("Down: %v", err)
	}

	version, err := Version(ctx, dsn)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	if version.Number != 0 || version.Dirty {
		t.Fatalf("version after Down = %+v, want 0 and clean", version)
	}

	if tableExists(t, pool, "notes") {
		t.Error("notes survived the down migration")
	}

	// Leave the database in the migrated state for whatever runs next.
	if _, err := Up(ctx, dsn); err != nil {
		t.Fatalf("final Up: %v", err)
	}
}

func openTestPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	pool, err := openPool(dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	t.Cleanup(func() { pool.Close() })

	return pool
}

func columnExists(t *testing.T, pool *sql.DB, table, column string) bool {
	t.Helper()

	var exists bool

	err := pool.QueryRowContext(t.Context(),
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = $1 AND column_name = $2
		)`, table, column,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}

	return exists
}

// highestEmbeddedVersion is the largest numbered up migration in the binary.
func highestEmbeddedVersion(t *testing.T) uint {
	t.Helper()

	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	var highest uint

	for _, entry := range entries {
		if number, ok := versionOf(entry.Name(), "up"); ok && number > highest {
			highest = number
		}
	}

	if highest == 0 {
		t.Fatal("no up migrations are embedded")
	}

	return highest
}

// indexState reports whether an index exists, whether the planner can use it, whether
// its predicate excludes part of the table, and which search states that predicate
// covers. The last one is checked rather than assumed because "is partial" says
// nothing about whether it is partial over the right rows: an index covering only
// 'pending' and 'failed' is partial too, and would still be correct as an index, while
// leaving every note stranded in 'indexing' by a dead worker unreadable by a scan.
func indexState(t *testing.T, pool *sql.DB, name string) (exists, valid, partial bool, states []string) {
	t.Helper()

	const inspect = `
		SELECT
			EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'i'),
			COALESCE(
				(SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = $1::regclass),
				false
			),
			COALESCE(
				(SELECT i.indpred IS NOT NULL FROM pg_index i WHERE i.indexrelid = $1::regclass),
				false
			),
			COALESCE(
				(SELECT pg_get_expr(i.indpred, i.indrelid) FROM pg_index i WHERE i.indexrelid = $1::regclass),
				''
			)`

	var predicate string

	if err := pool.QueryRowContext(t.Context(), inspect, name).
		Scan(&exists, &valid, &partial, &predicate); err != nil {
		t.Fatalf("check index %s: %v", name, err)
	}

	if !partial {
		return exists, valid, partial, nil
	}

	// Read as text and picked apart in Go rather than with a SQL regexp, so a
	// differently parenthesised but equivalent predicate still passes.
	const (
		marker    = "ARRAY["
		suffix    = "]"
		literal   = "'"
		castTo    = "::text"
		arrayOpen = "search_state = ANY ("
	)

	if !strings.Contains(predicate, arrayOpen) {
		t.Fatalf("index %s does not filter on search_state: predicate %q", name, predicate)
	}

	listed := predicate[strings.Index(predicate, marker)+len(marker):]
	listed = listed[:strings.LastIndex(listed, suffix)]

	for _, quoted := range strings.Split(listed, ",") {
		state := strings.TrimSpace(quoted)
		state = strings.TrimSuffix(state, castTo)
		states = append(states, strings.Trim(state, literal))
	}

	return exists, valid, partial, states
}

func tableExists(t *testing.T, pool *sql.DB, name string) bool {
	t.Helper()

	var exists bool

	err := pool.QueryRowContext(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`, name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}

	return exists
}

// databaseName pulls the database out of either a URL or a key/value DSN.
func databaseName(dsn string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Scheme != "" {
		return strings.TrimPrefix(parsed.Path, "/")
	}

	for _, field := range strings.Fields(dsn) {
		if name, found := strings.CutPrefix(field, "dbname="); found {
			return name
		}
	}

	return ""
}
