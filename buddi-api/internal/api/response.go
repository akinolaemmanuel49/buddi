package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// Error is an HTTP aware error. Handlers return it (or wrap it) and the
// router turns it into a JSON body with the matching status code.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any

	err error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return http.StatusText(e.Status)
}

func (e *Error) Unwrap() error { return e.err }

// WithDetails attaches field level context, e.g. validation errors.
func (e *Error) WithDetails(details any) *Error {
	e.Details = details
	return e
}

// WithCause keeps the underlying error for logging.
func (e *Error) WithCause(err error) *Error {
	e.err = err
	return e
}

func NewError(status int, code string, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func BadRequest(message string) *Error {
	return NewError(http.StatusBadRequest, "bad_request", message)
}

func Unauthorized(message string) *Error {
	return NewError(http.StatusUnauthorized, "unauthorized", message)
}

func Forbidden(message string) *Error {
	return NewError(http.StatusForbidden, "forbidden", message)
}

func NotFound(message string) *Error {
	return NewError(http.StatusNotFound, "not_found", message)
}

func Conflict(message string) *Error {
	return NewError(http.StatusConflict, "conflict", message)
}

func Unprocessable(message string) *Error {
	return NewError(http.StatusUnprocessableEntity, "validation_failed", message)
}

func Internal(message string) *Error {
	return NewError(http.StatusInternalServerError, "internal_error", message)
}

// GatewayTimeout reports that a dependency we call did not answer in time.
func GatewayTimeout(message string) *Error {
	return NewError(http.StatusGatewayTimeout, "gateway_timeout", message)
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes payload as JSON with the given status code.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}`))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(body)
}

// WriteError maps err onto a status code and writes the error body. Errors that
// are not an *Error are treated as internal failures and only their code and
// message are exposed.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error

	switch {
	case errors.As(err, &apiErr):
	case errors.Is(err, persistence.ErrNotFound):
		apiErr = NotFound("resource not found")
	case errors.Is(err, persistence.ErrConflict):
		apiErr = Conflict("resource already exists")
	case errors.Is(err, context.DeadlineExceeded):
		// The model runtime is a dependency, not part of this service, so running
		// out of time waiting for it is a 504 rather than a 500. The distinction
		// matters to a client deciding whether to retry.
		apiErr = GatewayTimeout("the model did not respond in time")
	case errors.Is(err, context.Canceled):
		// Nothing was wrong; the caller went away. Recorded for completeness, but
		// the response will not reach anyone.
		apiErr = NewError(499, "client_closed_request", "request cancelled")
	default:
		apiErr = Internal("an unexpected error occurred").WithCause(err)
	}

	if apiErr.Status == 0 {
		apiErr.Status = http.StatusInternalServerError
	}

	if apiErr.Code == "" {
		apiErr.Code = "error"
	}

	WriteJSON(w, r, apiErr.Status, errorResponse{Error: errorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Details:   apiErr.Details,
		RequestID: RequestIDFromContext(r.Context()),
	}})
}

// WriteValidationError writes a 422 with per field details.
func WriteValidationError(w http.ResponseWriter, r *http.Request, message string, details any) {
	WriteError(w, r, Unprocessable(message).WithDetails(details))
}

// WriteJSONError writes err with an explicit status, for the common cases where
// the code is known at the call site.
func WriteJSONError(w http.ResponseWriter, r *http.Request, status int, code string, message string) {
	WriteError(w, r, NewError(status, code, message))
}

// decodeJSON reads a size limited JSON body into dst and reports malformed or
// unknown input as a 400.
func decodeJSON(r *http.Request, dst any) error {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !isJSONContentType(contentType) {
		return BadRequest("content type must be application/json")
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return BadRequest("request body is required")
		case errors.As(err, new(*json.SyntaxError)):
			return BadRequest("request body contains malformed JSON").WithCause(err)
		case errors.As(err, new(*json.UnmarshalTypeError)):
			return BadRequest("request body has a field of the wrong type").WithCause(err)
		default:
			return BadRequest("request body could not be decoded").WithCause(err)
		}
	}

	if dec.More() {
		return BadRequest("request body must contain a single JSON object")
	}

	return nil
}

func isJSONContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")

	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}
