package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// timeLayout is the single timestamp format used in every response body, so
// clients only have to parse one shape.
const timeLayout = time.RFC3339

// The interfaces below are the API's view of the application layer. Depending on
// these rather than the concrete services keeps handler tests free of bcrypt,
// JWT signing and repositories.
type Authenticator interface {
	Register(ctx context.Context, input auth.RegisterInput) (*auth.Session, error)
	Login(ctx context.Context, email string, password string, meta auth.TokenMeta) (*auth.Session, error)
	Refresh(ctx context.Context, refreshToken string, meta auth.TokenMeta) (*auth.Session, error)
	Logout(ctx context.Context, refreshToken string) error
	Authenticate(ctx context.Context, accessToken string) (*domain.User, error)
}

type NoteService interface {
	Create(ctx context.Context, userID uuid.UUID, input note.CreateInput) (*domain.Note, error)
	Update(ctx context.Context, userID uuid.UUID, noteID uuid.UUID, input note.UpdateInput) (*domain.Note, error)
	List(ctx context.Context, userID uuid.UUID, filter domain.NoteFilter, page domain.Page) ([]domain.Note, int64, error)
	Get(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) (*domain.Note, error)
	Delete(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) error
}

type TaskService interface {
	Create(ctx context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error)
	Update(ctx context.Context, userID uuid.UUID, taskID uuid.UUID, input task.UpdateInput) (*domain.Task, error)
	SetStatus(ctx context.Context, userID uuid.UUID, taskID uuid.UUID, status domain.TaskStatus) (*domain.Task, error)
	Complete(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error)
	List(ctx context.Context, userID uuid.UUID, filter domain.TaskFilter, page domain.Page) ([]domain.Task, int64, error)
	Get(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error)
	Delete(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) error
}

// AgentService is the API's view of the agent layer. Planning and execution are
// both synchronous, so every method here returns only once the work is done.
type AgentService interface {
	Plan(ctx context.Context, userID uuid.UUID, goal string) (*agent.Run, error)
	Get(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*agent.Run, error)
	List(ctx context.Context, userID uuid.UUID, filter domain.RunFilter, page domain.Page) (*agent.RunList, error)
	Cancel(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*agent.Run, error)
	Approve(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*agent.Run, error)
	Reject(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*agent.Run, error)
}

// ChatService is the API's view of the chat layer.
//
// Turn is the only method that streams, and it does so through the emitter rather
// than returning a channel, so the handler controls when each event reaches the
// wire. The read methods are ordinary and buffered.
type ChatService interface {
	Turn(
		ctx context.Context,
		userID uuid.UUID,
		turn chat.Turn,
		emit chat.Emitter,
	) (*chat.Result, error)

	ListConversations(
		ctx context.Context,
		userID uuid.UUID,
		limit int,
		offset int,
	) ([]domain.Conversation, int64, error)

	GetConversation(
		ctx context.Context,
		userID uuid.UUID,
		id uuid.UUID,
	) (*domain.Conversation, []domain.Message, error)

	DeleteConversation(ctx context.Context, userID uuid.UUID, id uuid.UUID) error
}

// TokenIssuer verifies access tokens. It is a separate dependency from the
// authenticator so the middleware can stay narrow.
type TokenVerifier interface {
	Verify(token string) (auth.Claims, error)
}

// userKey carries the authenticated user on a request context.
type userKey struct{}

// WithUser returns a context carrying the authenticated user.
func WithUser(ctx context.Context, user *domain.User) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

// UserFromContext returns the authenticated user, if the request was
// authenticated.
func UserFromContext(ctx context.Context) (*domain.User, bool) {
	user, ok := ctx.Value(userKey{}).(*domain.User)

	return user, ok
}

// MustUserID returns the authenticated user's id. It reports false rather than
// panicking, because a handler reached without the auth middleware is a routing
// bug that should surface as a test failure, not a crash in production.
func MustUserID(r *http.Request) (uuid.UUID, bool) {
	user, ok := UserFromContext(r.Context())
	if !ok || user == nil {
		return uuid.Nil, false
	}

	return user.ID, true
}

// tokenFromRequest pulls a bearer token out of the Authorization header.
func tokenFromRequest(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(strings.TrimSpace(scheme), "Bearer") {
		return ""
	}

	return strings.TrimSpace(token)
}

// tokenMetaFromRequest collects the diagnostic fields stored with a refresh
// token.
func tokenMetaFromRequest(r *http.Request) auth.TokenMeta {
	return auth.TokenMeta{
		UserAgent: r.Header.Get("User-Agent"),
		IPAddress: clientIP(r),
	}
}

// clientIP reads the address of the immediate peer. Forwarded headers are
// deliberately ignored: they are client controlled, and a local-first service
// sits behind no proxy that would set them meaningfully.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}

	return host
}
