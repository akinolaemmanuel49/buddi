package gormdb

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

var errNotConnected = errors.New("gormdb: not connected")

const postgresUniqueViolation = "23505"

// translate maps driver specific errors onto the sentinel errors of the
// persistence package so callers never need to import GORM to inspect them.
func translate(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return persistence.ErrNotFound

	case errors.Is(err, gorm.ErrDuplicatedKey):
		return persistence.ErrConflict
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == postgresUniqueViolation {
		return persistence.ErrConflict
	}

	return err
}
