package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// writeServiceError translates a domain or persistence error into an HTTP
// response. Handlers call this and return, so no error string from the lower
// layers is ever echoed to a client verbatim.
//
// The mapping is deliberately blunt on ownership: a missing row and a row owned
// by someone else both become 404, because a 403 would confirm that the id
// exists in another tenant.
func (a *API) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := serviceError(r, err)

	// An unrecognised error is a bug or an outage, and the client only ever sees a
	// generic message. It is logged here, with the request id that is in the response
	// body, or it is undiagnosable from the outside.
	if apiErr.Code == "internal_error" {
		a.log.ErrorContext(r.Context(), "unhandled service error",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", RequestIDFromContext(r.Context()),
		)
	}

	WriteError(w, r, apiErr.WithCause(err))
}

// serviceError maps a domain or persistence error onto an API error.
//
// It is separate from writeServiceError because a streamed response has already
// committed its status code and cannot change it: the failure has to travel in the
// event body instead. Sharing this mapping is what keeps the code and the wording
// identical between the two paths, so a client parses one error shape.
func serviceError(_ *http.Request, err error) *Error {
	var validation *domain.ValidationError

	switch {
	case errors.As(err, &validation):
		return Unprocessable(validation.Message).WithDetails(map[string]any{
			"fields": validation.Fields,
		})

	case errors.Is(err, domain.ErrNotFound), errors.Is(err, persistence.ErrNotFound):
		return NotFound("resource not found")

	case errors.Is(err, domain.ErrConflict), errors.Is(err, persistence.ErrConflict):
		return Conflict("resource already exists")

	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrTokenReuse):
		return Unauthorized("invalid or expired credentials")

	case errors.Is(err, domain.ErrStateTransition):
		return Conflict("the resource is not in a state that allows this change")

	case errors.Is(err, domain.ErrInvalid):
		return Unprocessable("the request was rejected")

	case errors.Is(err, context.DeadlineExceeded):
		// The model runtime is a dependency of this service. Out of time waiting for
		// it is a 504, so a client can tell "try again" apart from "we are broken"
		// without reading the message.
		return GatewayTimeout("the model did not respond in time")

	default:
		return Internal("an unexpected error occurred")
	}
}
