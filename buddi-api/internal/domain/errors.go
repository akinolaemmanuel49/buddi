package domain

import "errors"

// Sentinel errors the application layer maps onto transport concerns. Handlers
// never inspect these directly; they translate them into status codes.
var (
	// ErrNotFound means the entity does not exist, or does not belong to the
	// requesting user. The two cases are deliberately indistinguishable.
	ErrNotFound = errors.New("not found")

	// ErrConflict means a uniqueness rule was violated, such as a duplicate
	// email address.
	ErrConflict = errors.New("conflict")

	// ErrInvalid means an entity invariant or a supplied value was rejected.
	ErrInvalid = errors.New("invalid value")

	// ErrUnauthorized means credentials were missing, malformed or wrong.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrTokenReuse means a refresh token that was already spent was presented
	// again, which is treated as a compromise of the whole token family.
	ErrTokenReuse = errors.New("refresh token reuse detected")

	// ErrStateTransition means a status change was not allowed from the current
	// state, such as completing an already cancelled task.
	ErrStateTransition = errors.New("invalid state transition")

	// ErrStale means the row changed after it was read, so a write built from the
	// older copy was refused. It is not the caller's mistake and not a failure worth
	// reporting: the newer version is already stored, so the right response is to
	// discard the write.
	ErrStale = errors.New("stale write")
)

// ValidationError carries per field problems so callers can report them
// individually instead of failing on the first bad value.
type ValidationError struct {
	Message string
	Fields  map[string]string
}

func (e *ValidationError) Error() string {
	return e.Message
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Invalid builds a ValidationError with the given field messages.
func Invalid(message string, fields map[string]string) error {
	return &ValidationError{Message: message, Fields: fields}
}
