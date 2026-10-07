// Package api wires the HTTP surface of the service: routing, middleware,
// request decoding and response encoding. Handlers live here or in
// subpackages and only deal with HTTP concerns.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// Pinger is the readiness dependency, satisfied by the persistence database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Options carries the dependencies of the API.
type Options struct {
	Logger         *slog.Logger
	Database       Pinger
	AllowedOrigins []string

	Auth  Authenticator
	Notes NoteService
	Tasks TaskService
	Agent AgentService
	Chat  ChatService

	// Connections and the three OAuth collaborators below are nil when a deployment
	// has no connector configured. The handlers check for that rather than the routes
	// being absent, so the endpoints exist and report why they cannot help: a client
	// gets a clear answer instead of a 404 that looks like a bug.
	Connections ConnectionsService
	Authorizer  AuthorizationStarter
	States      StateCodec
	Exchanger   AuthorizationExchanger

	// WebOrigin is where the OAuth callback sends the browser back to. See
	// config.Config.WebOrigin.
	WebOrigin string
}

const readinessTimeout = 2 * time.Second

// API owns the route table and the middleware stack.
type API struct {
	mux            *http.ServeMux
	log            *slog.Logger
	db             Pinger
	allowedOrigins []string

	auth  Authenticator
	notes NoteService
	tasks TaskService
	agent AgentService
	chat  ChatService

	connections ConnectionsService
	authorizer  AuthorizationStarter
	states      StateCodec
	exchanger   AuthorizationExchanger
	webOrigin   string
}

// New builds the API and registers every route.
func New(opts Options) *API {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}

	a := &API{
		mux:            http.NewServeMux(),
		log:            log,
		db:             opts.Database,
		allowedOrigins: opts.AllowedOrigins,
		auth:           opts.Auth,
		notes:          opts.Notes,
		tasks:          opts.Tasks,
		agent:          opts.Agent,
		chat:           opts.Chat,
		connections:    opts.Connections,
		authorizer:     opts.Authorizer,
		states:         opts.States,
		exchanger:      opts.Exchanger,
		webOrigin:      opts.WebOrigin,
	}

	a.registerRoutes()

	return a
}

// Handler returns the fully decorated http.Handler.
func (a *API) Handler() http.Handler {
	return Chain(
		RequestID(),
		Logger(a.log),
		Recoverer(a.log),
		CORS(a.allowedOrigins),
	)(a.mux)
}

// Handle registers an additional route, e.g. a.Handle("GET /api/v1/ping", h).
func (a *API) Handle(pattern string, handler http.HandlerFunc) {
	a.mux.HandleFunc(pattern, handler)
}

// private registers a route behind the authentication middleware. Requiring auth
// per route rather than for a whole prefix is what keeps /auth/login reachable
// by a client that has no token yet.
func (a *API) private(pattern string, handler http.HandlerFunc) {
	a.mux.Handle(pattern, a.Authenticate()(handler))
}

// registerRoutes is the single place where endpoints are declared. Resource
// routes belong under the /api/v1 prefix, operational routes stay at the root.
func (a *API) registerRoutes() {
	a.mux.HandleFunc("GET /healthz", a.handleHealth)
	a.mux.HandleFunc("GET /readyz", a.handleReady)

	a.registerV1Routes()

	a.mux.HandleFunc("/", a.handleNotFound)
}

// registerV1Routes holds the versioned resource routes. Add them here so the
// whole surface stays visible in one place.
func (a *API) registerV1Routes() {
	// Public. These are the only endpoints an anonymous caller may reach.
	a.mux.HandleFunc("POST /api/v1/auth/register", a.handleRegister)
	a.mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	a.mux.HandleFunc("POST /api/v1/auth/refresh", a.handleRefresh)
	a.mux.HandleFunc("POST /api/v1/auth/logout", a.handleLogout)

	a.private("GET /api/v1/auth/me", a.handleMe)

	a.private("POST /api/v1/notes", a.handleCreateNote)
	a.private("GET /api/v1/notes", a.handleListNotes)
	a.private("GET /api/v1/notes/{id}", a.handleGetNote)
	a.private("PATCH /api/v1/notes/{id}", a.handleUpdateNote)
	a.private("DELETE /api/v1/notes/{id}", a.handleDeleteNote)

	a.private("POST /api/v1/tasks", a.handleCreateTask)
	a.private("GET /api/v1/tasks", a.handleListTasks)
	a.private("GET /api/v1/tasks/{id}", a.handleGetTask)
	a.private("PATCH /api/v1/tasks/{id}", a.handleUpdateTask)
	a.private("POST /api/v1/tasks/{id}/complete", a.handleCompleteTask)
	a.private("DELETE /api/v1/tasks/{id}", a.handleDeleteTask)

	a.private("POST /api/v1/agent/runs", a.handleCreateRun)
	a.private("GET /api/v1/agent/runs", a.handleListRuns)
	a.private("GET /api/v1/agent/runs/{id}", a.handleGetRun)
	a.private("POST /api/v1/agent/runs/{id}/cancel", a.handleCancelRun)

	// Approvals are their own resource because each decision is independently
	// idempotent: deciding the same approval twice is a conflict, not a rerun.
	a.private("POST /api/v1/agent/approvals/{id}/approve", a.handleApprove)
	a.private("POST /api/v1/agent/approvals/{id}/reject", a.handleReject)

	// Chat is the conversational surface.
	//
	// Posting a message streams the reply rather than returning a resource, so the
	// turn route is separate from the collection: there is no body to respond with
	// when the whole answer is the response. Starting a new thread and appending to
	// one are the same call, distinguished by conversation_id, so they share a route
	// rather than having two that differ only in that field.
	a.private("POST /api/v1/chat/messages", a.handleStreamTurn)
	a.private("GET /api/v1/chat/conversations", a.handleListConversations)
	a.private("GET /api/v1/chat/conversations/{id}", a.handleGetConversation)
	a.private("DELETE /api/v1/chat/conversations/{id}", a.handleDeleteConversation)

	// Connections. The callback is public because it is where Google returns the user
	// with no Buddi session; it authenticates with the signed state instead.
	a.private("GET /api/v1/connections/{provider}", a.handleGetConnection)
	a.private("POST /api/v1/connections/{provider}", a.handleStartConnection)
	a.private("DELETE /api/v1/connections/{provider}", a.handleDeleteConnection)
	a.mux.HandleFunc("GET /api/v1/connections/{provider}/callback", a.handleCompleteConnection)
}

func (a *API) handleNotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, NotFound("no route matches "+r.URL.Path))
}
