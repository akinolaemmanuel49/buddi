package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/googleoauth"
)

// ConnectionsService is the API's view of the connection layer.
type ConnectionsService interface {
	// Status reports whether a provider is connected, without decrypting anything.
	Status(ctx context.Context, userID uuid.UUID, provider domain.Provider) (oauth.Connection, error)

	// Connect stores a freshly granted credential.
	Connect(ctx context.Context, userID uuid.UUID, provider domain.Provider, grant oauth.Grant) error

	// Disconnect revokes a stored credential.
	Disconnect(ctx context.Context, userID uuid.UUID, provider domain.Provider) error
}

// AuthorizationStarter begins an authorization round trip by producing the URL to
// send the user to.
type AuthorizationStarter interface {
	// AuthCodeURL returns the consent URL for state.
	AuthCodeURL(state string) string
}

// AuthorizationExchanger trades an authorization code for tokens.
type AuthorizationExchanger interface {
	Exchange(ctx context.Context, code string) (*oauth.Grant, error)
}

// StateCodec issues and verifies the state parameter.
type StateCodec interface {
	Issue(userID uuid.UUID) (string, error)
	Verify(value string) (googleoauth.State, error)
}

type connectionResponse struct {
	Provider  string   `json:"provider"`
	Connected bool     `json:"connected"`
	Scopes    []string `json:"scopes,omitempty"`
	ExpiresAt *string  `json:"expires_at,omitempty"`
}

type connectResponse struct {
	Provider string `json:"provider"`
	// AuthorizeURL is where the client sends the browser.
	AuthorizeURL string `json:"authorize_url"`
}

// handleGetConnection reports whether a provider is connected.
func (a *API) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	provider, ok := providerFromPath(w, r)
	if !ok {
		return
	}

	if a.connections == nil {
		WriteError(w, r, Unprocessable(
			"this deployment has no connectors configured"))

		return
	}

	connection, err := a.connections.Status(r.Context(), userID, provider)
	if err != nil {
		// "Not connected" and "revoked" are answers, not failures. The service reports
		// them as sentinels because it is also used where a caller genuinely needs a
		// token, but over HTTP the question was "is this connected?", and a 500 for
		// the ordinary unconnected case would make the connector look broken instead
		// of merely unused.
		if errors.Is(err, oauth.ErrNotConnected) || errors.Is(err, oauth.ErrRevoked) {
			WriteJSON(w, r, http.StatusOK, toConnectionResponse(oauth.Connection{
				Provider: provider,
			}))

			return
		}

		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toConnectionResponse(connection))
}

// handleStartConnection returns the consent URL for a provider.
//
// It responds with a URL rather than redirecting, because the client is a single
// page app: a 302 to Google would navigate away from the app, and on the way back
// the app would reload and lose whatever it was showing. Returning the URL lets the
// client open it in the same tab deliberately and come back to a live app.
func (a *API) handleStartConnection(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	provider, ok := providerFromPath(w, r)
	if !ok {
		return
	}

	if a.authorizer == nil || a.states == nil {
		WriteError(w, r, Unprocessable(
			"this deployment has no Google credentials configured, so the connector cannot be used"))

		return
	}

	state, err := a.states.Issue(userID)
	if err != nil {
		a.log.ErrorContext(r.Context(), "could not issue authorization state", "error", err)

		WriteError(w, r, Internal("could not start the connection"))

		return
	}

	WriteJSON(w, r, http.StatusOK, connectResponse{
		Provider:     string(provider),
		AuthorizeURL: a.authorizer.AuthCodeURL(state),
	})
}

// handleCompleteConnection finishes an authorization and stores the credential.
//
// This route is deliberately not behind the auth middleware. Google returns the user
// here with no Buddi session: the redirect is a fresh navigation and the access token
// lives in localStorage, which is not sent on a cross-site navigation. Identity comes
// from the signed state instead, which is why the state is signed.
//
// Every outcome redirects back to the app rather than answering with JSON, because
// every outcome is something a user watching a browser will reach. The detail goes to
// the log, where it is diagnosable, and the app gets a code it can put in words.
func (a *API) handleCompleteConnection(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Google's own error comes back as query parameters, so it has to be read before
	// concluding the code is missing.
	if denied := query.Get("error"); denied != "" {
		detail := query.Get("error_description")

		a.log.InfoContext(r.Context(), "user did not complete authorization",
			"error", denied, "description", detail)

		a.redirectToApp(w, r, "calendar", "error", denied)

		return
	}

	code := query.Get("code")
	if code == "" {
		a.log.InfoContext(r.Context(), "authorization callback carried no code")

		a.redirectToApp(w, r, "calendar", "error", "no_code")

		return
	}

	if a.states == nil || a.exchanger == nil || a.connections == nil {
		a.log.InfoContext(r.Context(), "authorization callback with no connector configured")

		a.redirectToApp(w, r, "calendar", "error", "not_configured")

		return
	}

	state, err := a.states.Verify(query.Get("state"))
	if err != nil {
		// Logged with the reason and reported to the user only as "start again".
		// Echoing which check failed would tell an attacker whether a guessed state was
		// well formed, wrong signature, or merely expired.
		a.log.InfoContext(r.Context(), "rejected an authorization callback", "reason", err.Error())

		a.redirectToApp(w, r, "calendar", "error", "unverified")

		return
	}

	grant, err := a.exchanger.Exchange(r.Context(), code)
	if err != nil {
		a.log.ErrorContext(r.Context(), "authorization code exchange failed", "error", err)

		a.redirectToApp(w, r, "calendar", "error", "exchange_failed")

		return
	}

	if err := a.connections.Connect(r.Context(), state.UserID, googleoauth.ProviderName, *grant); err != nil {
		a.log.ErrorContext(r.Context(), "could not store the calendar credential", "error", err)

		a.redirectToApp(w, r, "calendar", "error", "storage_failed")

		return
	}

	a.log.InfoContext(r.Context(), "calendar connected", "user_id", state.UserID)

	a.redirectToApp(w, r, "calendar", "connected", "")
}

// redirectToApp sends the browser back to the web app with an outcome in the query.
//
// The query is the whole message: there is no session on this navigation to read
// anything from, and adding one would mean inventing a session for a route that only
// ever runs once per connection.
func (a *API) redirectToApp(w http.ResponseWriter, r *http.Request, key, value, reason string) {
	target, err := url.Parse(a.webOrigin)
	if err != nil {
		WriteError(w, r, Internal("this deployment has no usable web origin configured"))

		return
	}

	query := target.Query()
	query.Set(key, value)

	if reason != "" {
		query.Set("reason", reason)
	}

	target.RawQuery = query.Encode()

	// 303 rather than 302 so the browser follows with GET even if the original
	// request was anything else, which keeps the app's URL clean.
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// handleDeleteConnection removes a stored credential.
func (a *API) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	provider, ok := providerFromPath(w, r)
	if !ok {
		return
	}

	if err := a.connections.Disconnect(r.Context(), userID, provider); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// providerFromPath reads and validates the provider segment.
func providerFromPath(w http.ResponseWriter, r *http.Request) (domain.Provider, bool) {
	raw := PathParam(r, "provider")
	provider := domain.Provider(raw)

	if !provider.IsValid() {
		WriteError(w, r, NotFound("no such provider"))

		return "", false
	}

	return provider, true
}

func toConnectionResponse(connection oauth.Connection) connectionResponse {
	response := connectionResponse{
		Provider:  string(connection.Provider),
		Connected: connection.Connected,
		Scopes:    connection.Scopes,
	}

	if !connection.ExpiresAt.IsZero() {
		expires := connection.ExpiresAt.Format(timeLayout)
		response.ExpiresAt = &expires
	}

	return response
}
