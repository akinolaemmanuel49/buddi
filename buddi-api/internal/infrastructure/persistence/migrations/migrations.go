// Package migrations embeds the SQL schema and applies it with golang-migrate.
// The migrations run through a dedicated connection pool so that closing the
// migrator can never take down the application's own pool.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxv5 "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// FS holds the embedded migration files.
//
//go:embed *.sql
var FS embed.FS

// SchemaVersion is the version currently recorded in the database.
type SchemaVersion struct {
	Number uint
	Dirty  bool
}

// Result reports what a migration run did.
type Result struct {
	Applied []string
	Version SchemaVersion
}

// Up applies every pending migration and returns the applied files plus the
// resulting version. A database already at the latest version is not an error.
//
// golang-migrate does not accept a context for Up, so ctx is honoured at entry
// only; an already-cancelled context aborts before any statement runs.
func Up(ctx context.Context, dsn string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	m, pool, err := newMigrator(dsn)
	if err != nil {
		return Result{}, err
	}

	defer closeMigrator(m, pool)

	before, err := currentVersion(ctx, pool)
	if err != nil {
		return Result{}, err
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return Result{}, fmt.Errorf("migrations: up: %w", err)
	}

	after, err := currentVersion(ctx, pool)
	if err != nil {
		return Result{}, err
	}

	return Result{Applied: diff(before, after), Version: after}, nil
}

// Down rolls back steps migrations, or all of them when steps is zero or less.
//
// golang-migrate's Steps takes a signed count, and only ever moves one version per
// call, so "all the way down" is a loop rather than a single call. The loop also
// treats ErrNoChange as the stopping condition: that is what a down at version 0
// returns, and it is success rather than a failure.
func Down(ctx context.Context, dsn string, steps int) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m, pool, err := newMigrator(dsn)
	if err != nil {
		return err
	}

	defer closeMigrator(m, pool)

	if steps > 0 {
		if err := m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrations: down: %w", err)
		}

		return nil
	}

	// Bounded by the number of migrations. The version is re-read each pass rather
	// than relying on the error from an over-step: at version 0 golang-migrate
	// reports a missing file rather than ErrNoChange, so an error-based exit test
	// treats the end of the migration list as a failure.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		version, err := currentVersion(ctx, pool)
		if err != nil {
			return err
		}

		if version.Number == 0 {
			return nil
		}

		if err := m.Steps(-1); err != nil {
			if errors.Is(err, migrate.ErrNoChange) || errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("migrations: down: %w", err)
		}
	}
}

// Version reports the recorded schema version.
func Version(ctx context.Context, dsn string) (SchemaVersion, error) {
	if err := ctx.Err(); err != nil {
		return SchemaVersion{}, err
	}

	pool, err := openPool(dsn)
	if err != nil {
		return SchemaVersion{}, err
	}

	defer pool.Close()

	return currentVersion(ctx, pool)
}

func newMigrator(dsn string) (*migrate.Migrate, *sql.DB, error) {
	pool, err := openPool(dsn)
	if err != nil {
		return nil, nil, err
	}

	src, err := iofs.New(FS, ".")
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("migrations: source: %w", err)
	}

	driver, err := pgxv5.WithInstance(pool, &pgxv5.Config{})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("migrations: driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgxv5", driver)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("migrations: new: %w", err)
	}

	return m, pool, nil
}

// closeMigrator releases the source and driver. The driver's Close also closes
// the pool it was given, which is why the pool is dedicated to migrations.
func closeMigrator(m *migrate.Migrate, pool *sql.DB) {
	if m != nil {
		_, _ = m.Close()
		return
	}

	if pool != nil {
		_ = pool.Close()
	}
}

func openPool(dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("migrations: dsn is required")
	}

	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("migrations: open pool: %w", err)
	}

	return pool, nil
}

func currentVersion(ctx context.Context, pool *sql.DB) (SchemaVersion, error) {
	var version SchemaVersion

	// schema_migrations is created by golang-migrate on first use, so a missing
	// table simply means nothing has been applied yet.
	row := pool.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`)

	err := row.Scan(&version.Number, &version.Dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SchemaVersion{}, nil
	case err != nil:
		return SchemaVersion{}, fmt.Errorf("migrations: read version: %w", err)
	}

	return version, nil
}

// diff lists the migration files applied between two versions.
func diff(before, after SchemaVersion) []string {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return nil
	}

	var applied []string

	for _, entry := range entries {
		name := entry.Name()

		version, ok := versionOf(name, "up")
		if !ok {
			continue
		}

		if version > before.Number && version <= after.Number {
			applied = append(applied, name)
		}
	}

	sort.Strings(applied)

	return applied
}

// versionOf extracts the migration number from a file name such as
// 000001_init.up.sql. It reports false for anything that is not a migration
// file in the requested direction.
func versionOf(name string, direction string) (uint, bool) {
	if !strings.HasSuffix(name, "."+direction+".sql") {
		return 0, false
	}

	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, false
	}

	number, err := strconv.ParseUint(prefix, 10, 32)
	if err != nil {
		return 0, false
	}

	return uint(number), true
}
