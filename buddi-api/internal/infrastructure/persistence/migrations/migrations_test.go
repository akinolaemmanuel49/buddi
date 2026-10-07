package migrations

import (
	"context"
	"io/fs"
	"strings"
	"testing"
)

// The migration files are embedded, so a malformed name fails at runtime rather
// than at compile time. This test keeps that failure local.

func TestEmbeddedFilesArePairedAndVersioned(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("no embedded migrations found")
	}

	up := map[uint]string{}
	down := map[uint]bool{}

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			t.Fatalf("unexpected directory in migrations: %s", name)
		}

		if !strings.HasSuffix(name, ".sql") {
			t.Fatalf("unexpected non-SQL file in migrations: %s", name)
		}

		switch {
		case strings.HasSuffix(name, ".up.sql"):
			version, ok := versionOf(name, "up")
			if !ok {
				t.Fatalf("cannot parse version from %s", name)
			}

			if previous, seen := up[version]; seen {
				t.Fatalf("duplicate migration %d: %s and %s", version, previous, name)
			}

			up[version] = name
		case strings.HasSuffix(name, ".down.sql"):
			version, ok := versionOf(name, "down")
			if !ok {
				t.Fatalf("cannot parse version from %s", name)
			}

			down[version] = true
		default:
			t.Fatalf("migration %s must end in .up.sql or .down.sql", name)
		}

		content, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		if len(strings.TrimSpace(string(content))) == 0 {
			t.Fatalf("migration %s is empty", name)
		}
	}

	for version, name := range up {
		if !down[version] {
			t.Fatalf("migration %s (%d) has no matching down file", name, version)
		}
	}

	for version := range down {
		if _, ok := up[version]; !ok {
			t.Fatalf("down migration %d has no matching up file", version)
		}
	}
}

func TestVersionOf(t *testing.T) {
	cases := []struct {
		name      string
		direction string
		want      uint
		wantOK    bool
	}{
		{"000001_init.up.sql", "up", 1, true},
		{"000042_add_index.down.sql", "down", 42, true},
		{"README.md", "up", 0, false},
		{"000001_init.down.sql", "up", 0, false},
		{"init.up.sql", "up", 0, false},
		{"abc_init.up.sql", "up", 0, false},
	}

	for _, tc := range cases {
		got, ok := versionOf(tc.name, tc.direction)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("versionOf(%q, %q) = (%d, %v), want (%d, %v)", tc.name, tc.direction, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestUpRejectsEmptyDSN(t *testing.T) {
	if _, err := Up(t.Context(), ""); err == nil {
		t.Fatal("expected an error for an empty dsn")
	}
}

func TestUpHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := Up(ctx, "postgres://localhost:1/none"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
