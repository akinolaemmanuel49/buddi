package api

import (
	"net/http"

	"github.com/google/uuid"
)

// Authenticate rejects requests without a valid access token and puts the
// resolved user on the request context.
//
// A missing header and an invalid token produce the same 401 with the same
// body, so the endpoint does not tell an anonymous caller which failure mode
// they hit.
func (a *API) Authenticate() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := tokenFromRequest(r)
			if token == "" {
				WriteError(w, r, Unauthorized("an access token is required"))

				return
			}

			user, err := a.auth.Authenticate(r.Context(), token)
			if err != nil {
				a.writeServiceError(w, r, err)

				return
			}

			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
		})
	}
}

// requireUser is the guard every authenticated handler calls first. The
// middleware above should make it unreachable, so a failure here is reported as
// an internal error rather than a 401 that would suggest the client's token was
// the problem.
func requireUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := MustUserID(r)
	if !ok {
		WriteError(w, r, Internal("authenticated route reached without a user on the context"))

		return uuid.Nil, false
	}

	return userID, true
}
