package api

import (
	"net/http"
	"strconv"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// maxRequestBodyBytes caps request bodies at 1 MiB.
const maxRequestBodyBytes = 1 << 20

// maxLimit caps the page size a client may request.
const maxLimit = 200

// Query returns a trimmed query string parameter.
func Query(r *http.Request, name string) string {
	return r.URL.Query().Get(name)
}

// QueryInt returns a query parameter as an int, or fallback when absent.
func QueryInt(r *http.Request, name string, fallback int) int {
	raw := Query(r, name)
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}

	return value
}

// QueryInt64 returns a query parameter as an int64, or fallback when absent.
func QueryInt64(r *http.Request, name string, fallback int64) int64 {
	raw := Query(r, name)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}

	return value
}

// QueryBool returns a query parameter as a bool, or fallback when absent.
func QueryBool(r *http.Request, name string, fallback bool) bool {
	raw := Query(r, name)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}

	return value
}

// Pagination reads limit, offset and order query parameters and returns list
// options bounded by maxLimit. Out of range or malformed values are reported as
// a 400 instead of being silently ignored.
func Pagination(r *http.Request) (persistence.ListOptions, error) {
	opts := persistence.ListOptions{Limit: persistence.DefaultLimit}

	if raw := Query(r, "limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxLimit {
			return opts, BadRequest("limit must be between 1 and " + strconv.Itoa(maxLimit))
		}

		opts.Limit = limit
	}

	if raw := Query(r, "offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return opts, BadRequest("offset must be zero or greater")
		}

		opts.Offset = offset
	}

	opts.Order = Query(r, "order")

	return opts, nil
}

// PathParam returns a path wildcard, e.g. r.PathValue("id") for
// "GET /api/v1/users/{id}".
func PathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}
