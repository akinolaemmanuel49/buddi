package persistence

import (
	"errors"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// The not-found and conflict failures are the domain's vocabulary, not this
// package's. They are aliased rather than redefined so that an error raised by
// the generic repository and an error raised by a typed repository are the same
// value, and errors.Is works identically for application services, which only
// import the domain, and for the API layer, which also sees persistence errors
// from the generic base repository.
var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = domain.ErrNotFound

	// ErrConflict is returned when a write violates a uniqueness constraint.
	ErrConflict = domain.ErrConflict

	// ErrStale is returned when the row changed after it was read.
	ErrStale = domain.ErrStale

	// ErrTxDone is returned when a transaction is committed or rolled back twice.
	ErrTxDone = errors.New("transaction already finished")
)

// IsNotFound reports whether err signals a missing row.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// IsConflict reports whether err signals a uniqueness violation.
func IsConflict(err error) bool {
	return errors.Is(err, ErrConflict)
}

// IsStale reports whether err signals a write built from a copy the row has since
// moved past.
func IsStale(err error) bool {
	return errors.Is(err, ErrStale)
}
