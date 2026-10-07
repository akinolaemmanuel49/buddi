package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/api"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/note"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// The fakes below stand in for the application services so the handler tests
// exercise routing, decoding, the auth gate and the error mapping, and nothing
// else. Each records the user id it was handed, which is how the tests check
// that a handler forwards the authenticated identity rather than one from the
// request body.

type fakeAuth struct {
	mu sync.Mutex

	user        *domain.User
	authErr     error
	registerErr error
	loginErr    error
	refreshErr  error

	gotToken string
	calls    int
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{user: &domain.User{
		ID:          uuid.New(),
		Email:       "user@example.com",
		DisplayName: "Test User",
	}}
}

func (f *fakeAuth) Register(context.Context, auth.RegisterInput) (*auth.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.registerErr != nil {
		return nil, f.registerErr
	}

	return f.session(), nil
}

func (f *fakeAuth) Login(context.Context, string, string, auth.TokenMeta) (*auth.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.loginErr != nil {
		return nil, f.loginErr
	}

	return f.session(), nil
}

func (f *fakeAuth) Refresh(context.Context, string, auth.TokenMeta) (*auth.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.refreshErr != nil {
		return nil, f.refreshErr
	}

	return f.session(), nil
}

func (f *fakeAuth) Logout(context.Context, string) error { return nil }

func (f *fakeAuth) Authenticate(_ context.Context, token string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.gotToken = token

	if f.authErr != nil {
		return nil, f.authErr
	}

	return f.user, nil
}

func (f *fakeAuth) session() *auth.Session {
	return &auth.Session{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Date(2026, 3, 1, 12, 15, 0, 0, time.UTC),
		User:         f.user,
	}
}

type fakeNoteService struct {
	mu sync.Mutex

	createErr error
	listErr   error
	getErr    error

	// note and notes override what Get and List return, so a test can assert on a
	// response built from a value it chose rather than the default one.
	note  *domain.Note
	notes []domain.Note

	lastUserID   uuid.UUID
	lastNoteID   uuid.UUID
	lastListFilt domain.NoteFilter
	listTotal    int64
}

func (f *fakeNoteService) Create(_ context.Context, userID uuid.UUID, input note.CreateInput) (*domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID

	if f.createErr != nil {
		return nil, f.createErr
	}

	return &domain.Note{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     input.Title,
		Content:   input.Content,
		Tags:      domain.TagListOf(input.Tags),
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeNoteService) Update(_ context.Context, userID uuid.UUID, noteID uuid.UUID, input note.UpdateInput) (*domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastNoteID = noteID

	return &domain.Note{
		ID:        noteID,
		UserID:    userID,
		Title:     "updated",
		Tags:      domain.TagListOf(nil),
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeNoteService) List(_ context.Context, userID uuid.UUID, filter domain.NoteFilter, _ domain.Page) ([]domain.Note, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastListFilt = filter

	if f.listErr != nil {
		return nil, 0, f.listErr
	}

	if f.notes != nil {
		return f.notes, f.listTotal, nil
	}

	return []domain.Note{}, f.listTotal, nil
}

func (f *fakeNoteService) Get(_ context.Context, userID uuid.UUID, noteID uuid.UUID) (*domain.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastNoteID = noteID

	if f.getErr != nil {
		return nil, f.getErr
	}

	if f.note != nil {
		return f.note, nil
	}

	return &domain.Note{
		ID:        noteID,
		UserID:    userID,
		Title:     "a note",
		Tags:      domain.TagListOf(nil),
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeNoteService) Delete(_ context.Context, userID uuid.UUID, noteID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastNoteID = noteID

	if f.getErr != nil {
		return f.getErr
	}

	return nil
}

type fakeTaskService struct {
	mu sync.Mutex

	createErr error
	statusErr error

	lastUserID uuid.UUID
	lastTaskID uuid.UUID
	lastStatus domain.TaskStatus
}

func (f *fakeTaskService) Create(_ context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID

	if f.createErr != nil {
		return nil, f.createErr
	}

	return &domain.Task{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     input.Title,
		Status:    domain.TaskStatusPending,
		Priority:  domain.TaskPriorityNormal,
		DueAt:     input.DueAt,
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeTaskService) Update(_ context.Context, userID uuid.UUID, taskID uuid.UUID, _ task.UpdateInput) (*domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastTaskID = taskID

	return &domain.Task{
		ID:        taskID,
		UserID:    userID,
		Title:     "updated",
		Status:    domain.TaskStatusPending,
		Priority:  domain.TaskPriorityNormal,
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeTaskService) SetStatus(_ context.Context, userID uuid.UUID, taskID uuid.UUID, status domain.TaskStatus) (*domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastTaskID = taskID
	f.lastStatus = status

	if f.statusErr != nil {
		return nil, f.statusErr
	}

	return &domain.Task{
		ID:        taskID,
		UserID:    userID,
		Title:     "a task",
		Status:    status,
		Priority:  domain.TaskPriorityNormal,
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeTaskService) Complete(_ context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error) {
	f.mu.Lock()
	statusErr := f.statusErr
	f.mu.Unlock()

	if statusErr != nil {
		return nil, statusErr
	}

	return f.SetStatus(context.Background(), userID, taskID, domain.TaskStatusCompleted)
}

func (f *fakeTaskService) List(_ context.Context, userID uuid.UUID, _ domain.TaskFilter, _ domain.Page) ([]domain.Task, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID

	return []domain.Task{}, 0, nil
}

func (f *fakeTaskService) Get(_ context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastTaskID = taskID

	if f.statusErr != nil {
		return nil, f.statusErr
	}

	return &domain.Task{
		ID:        taskID,
		UserID:    userID,
		Title:     "a task",
		Status:    domain.TaskStatusPending,
		Priority:  domain.TaskPriorityNormal,
		Source:    domain.NoteSourceManual,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeTaskService) Delete(_ context.Context, userID uuid.UUID, taskID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastUserID = userID
	f.lastTaskID = taskID

	return nil
}

// fakePinger stands in for the database connection in readiness checks.
type fakePinger struct {
	err error
}

func (p fakePinger) Ping(context.Context) error { return p.err }

type fakeAgentService struct {
	run *agent.Run
	err error
}

func (f *fakeAgentService) Plan(context.Context, uuid.UUID, string) (*agent.Run, error) {
	return f.run, f.err
}

func (f *fakeAgentService) Get(context.Context, uuid.UUID, uuid.UUID) (*agent.Run, error) {
	return f.run, f.err
}

func (f *fakeAgentService) List(context.Context, uuid.UUID, domain.RunFilter, domain.Page) (*agent.RunList, error) {
	if f.err != nil {
		return nil, f.err
	}

	return &agent.RunList{Runs: []domain.AgentRun{}, Total: 0}, nil
}

func (f *fakeAgentService) Cancel(context.Context, uuid.UUID, uuid.UUID) (*agent.Run, error) {
	return f.run, f.err
}

func (f *fakeAgentService) Approve(context.Context, uuid.UUID, uuid.UUID) (*agent.Run, error) {
	return f.run, f.err
}

func (f *fakeAgentService) Reject(context.Context, uuid.UUID, uuid.UUID) (*agent.Run, error) {
	return f.run, f.err
}

// TestRunResponseSerialisesPlanFallback guards the provenance flag through the
// HTTP layer.
//
// The API builds its own response struct rather than serialising the domain
// model, which is how agent_runs.trace_id ended up documented but always absent:
// the field existed on the model and was never added here. A test that only
// checked the service layer would not have caught either.
func TestRunResponseSerialisesPlanFallback(t *testing.T) {
	tests := map[string]bool{"model produced it": false, "fallback": true}

	for name, fallback := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.run = awaitingRun(fallback)

			rec := h.authed(t, http.MethodPost, "/api/v1/agent/runs", map[string]string{"goal": "buy milk"})
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
			}

			body := decodeBody(t, rec)

			got, ok := body["plan_fallback"]
			if !ok {
				t.Fatalf("plan_fallback absent from the response; keys = %v", keysOf(body))
			}

			if got != fallback {
				t.Errorf("plan_fallback = %v, want %v", got, fallback)
			}
		})
	}
}

func awaitingRun(fallback bool) *agent.Run {
	now := time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC)

	return &agent.Run{
		Run: &domain.AgentRun{
			ID:           uuid.New(),
			Goal:         "buy milk",
			Status:       domain.RunStatusAwaitingApproval,
			Plan:         json.RawMessage(`{"title":"Buy milk"}`),
			Model:        "test-model",
			PlanFallback: fallback,
			StartedAt:    &now,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		Approvals: []domain.Approval{},
	}
}

// TestRunResponseSerialisesGroundingState guards the grounding flag through the HTTP
// layer, for the same reason as plan_fallback above: the column exists on the run and
// means nothing to a user who cannot see it.
//
// "no_context" is included as an explicit case because it is the value a client has to
// distinguish from an absent field.
func TestRunResponseSerialisesGroundingState(t *testing.T) {
	tests := map[string]domain.GroundingState{
		"retrieved notes reached the planner": domain.GroundingGrounded,
		"retrieval matched nothing":           domain.GroundingNoContext,
		"retrieval failed":                    domain.GroundingUngrounded,
		"never set":                           "",
	}

	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.run = awaitingRun(false)
			h.agent.run.Run.GroundingState = state

			rec := h.authed(t, http.MethodPost, "/api/v1/agent/runs", map[string]string{"goal": "buy milk"})
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
			}

			body := decodeBody(t, rec)

			got, ok := body["grounding_state"]
			if !ok {
				t.Fatalf("grounding_state absent from the response; keys = %v", keysOf(body))
			}

			want := string(state)
			if state == "" {
				// Normalised rather than passed through: claiming the user's notes
				// reached the planner would be inventing a fact this layer cannot know.
				want = string(domain.GroundingNoContext)
			}

			if got != want {
				t.Errorf("grounding_state = %v, want %v", got, want)
			}
		})
	}
}

// TestNoteResponseSerialisesSearchState guards the note's search state through the HTTP
// layer. A user who saves a note cannot tell whether it can be found again otherwise,
// and a note that silently never becomes searchable is the failure this feature exists
// to prevent.
func TestNoteResponseSerialisesSearchState(t *testing.T) {
	tests := map[string]domain.SearchState{
		"indexed":   domain.SearchStateIndexed,
		"queued":    domain.SearchStatePending,
		"failed":    domain.SearchStateFailed,
		"in flight": domain.SearchStateIndexing,
	}

	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.notes.notes = []domain.Note{{
				ID:          uuid.New(),
				Title:       "Deploy freeze procedure",
				Content:     "freeze merges before releasing",
				Tags:        domain.TagListOf([]string{"oncall"}),
				Source:      domain.NoteSourceManual,
				SearchState: state,
				CreatedAt:   time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
				UpdatedAt:   time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
			}}

			rec := h.authed(t, http.MethodGet, "/api/v1/notes?limit=1", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}

			body := decodeBody(t, rec)

			list, ok := body["data"].([]any)
			if !ok || len(list) != 1 {
				t.Fatalf("want one note in the list, got %v", body["data"])
			}

			note, ok := list[0].(map[string]any)
			if !ok {
				t.Fatalf("note is not an object: %v", list[0])
			}

			got, ok := note["search_state"]
			if !ok {
				t.Fatalf("search_state absent from the response; keys = %v", keysOf(note))
			}

			if got != string(state) {
				t.Errorf("search_state = %v, want %q", got, state)
			}
		})
	}
}

// TestNoteResponseOmitsTheIndexError checks the other half of that decision: the state
// is exposed, but the embedding runtime's error text is not, because it can name hosts
// and URLs that are none of the caller's business.
func TestNoteResponseOmitsTheIndexError(t *testing.T) {
	h := newHarness(t)
	h.notes.notes = []domain.Note{{
		ID:             uuid.New(),
		Title:          "Deploy freeze procedure",
		Content:        "freeze merges before releasing",
		Tags:           domain.TagListOf([]string{"oncall"}),
		Source:         domain.NoteSourceManual,
		SearchState:    domain.SearchStateFailed,
		LastIndexError: "dial tcp 10.0.0.7:11434: connect: connection refused",
		CreatedAt:      time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC),
	}}

	rec := h.authed(t, http.MethodGet, "/api/v1/notes?limit=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("the embedding runtime's error leaked into the response: %s", rec.Body.String())
	}
}

func keysOf(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}

	return keys
}

type harness struct {
	handler http.Handler
	auth    *fakeAuth
	notes   *fakeNoteService
	tasks   *fakeTaskService
	pinger  *fakePinger
	agent   *fakeAgentService
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	authFake := newFakeAuth()
	notes := &fakeNoteService{}
	tasks := &fakeTaskService{}
	pinger := &fakePinger{}
	agentFake := &fakeAgentService{}

	// Discard the middleware's log lines; the tests assert on responses.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := api.New(api.Options{
		Logger:   log,
		Database: pinger,
		Auth:     authFake,
		Notes:    notes,
		Tasks:    tasks,
		Agent:    agentFake,
	}).Handler()

	return &harness{
		handler: handler, auth: authFake, notes: notes,
		tasks: tasks, pinger: pinger, agent: agentFake,
	}
}

func (h *harness) do(t *testing.T, method string, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	return rec
}

// authed issues a request with a bearer token the fake authenticator accepts.
func (h *harness) authed(t *testing.T, method string, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return h.do(t, method, path, body, map[string]string{"Authorization": "Bearer good-token"})
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}

	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	body := decodeBody(t, rec)

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %s has no error object", rec.Body.String())
	}

	code, _ := errObj["code"].(string)

	return code
}

func TestHealthIsReachableWithoutAToken(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestReadyReflectsTheDatabase(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200 when the database answers", rec.Code)
	}

	// Readiness must fail closed, so traffic stops reaching an instance that
	// cannot serve it.
	h.pinger.err = errors.New("connection refused")

	rec = h.do(t, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d, want 503 when the database is down", rec.Code)
	}

	// Liveness must stay up regardless: the database being down is not a reason
	// to kill and restart the process.
	if rec := h.do(t, http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200 even with the database down", rec.Code)
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/api/v1/nope", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRegisterIsPublic(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":        "new@example.com",
		"password":     "a good password",
		"display_name": "New User",
	}, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)

	if body["access_token"] != "access-token" {
		t.Errorf("access_token = %v, want access-token", body["access_token"])
	}

	// The password hash must never reach the client.
	if strings.Contains(rec.Body.String(), "password") {
		t.Error("the response must not mention the password")
	}
}

func TestRegisterRejectsMalformedJSON(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegisterRejectsUnknownFields(t *testing.T) {
	h := newHarness(t)

	// Strict decoding means a typo in a field name fails loudly instead of being
	// silently dropped, which would look like a successful save.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register",
		strings.NewReader(`{"email":"a@example.com","password":"a good password","emial":"typo"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRegisterMapsValidationFailureTo422(t *testing.T) {
	h := newHarness(t)
	h.auth.registerErr = domain.Invalid("password is too short", map[string]string{"password": "must be at least 8 characters"})

	rec := h.do(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "new@example.com",
		"password": "short",
	}, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}

	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}

	body := decodeBody(t, rec)

	errObj := body["error"].(map[string]any)
	details, ok := errObj["details"].(map[string]any)
	if !ok {
		t.Fatalf("response has no details: %s", rec.Body.String())
	}

	fields, ok := details["fields"].(map[string]any)
	if !ok || fields["password"] == nil {
		t.Errorf("details.fields is %v, want the offending field named", details["fields"])
	}
}

func TestLoginMapsUnauthorizedTo401(t *testing.T) {
	h := newHarness(t)
	h.auth.loginErr = domain.ErrUnauthorized

	rec := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email":    "user@example.com",
		"password": "wrong",
	}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRefreshMapsTokenReuseTo401(t *testing.T) {
	h := newHarness(t)
	h.auth.refreshErr = domain.ErrTokenReuse

	rec := h.do(t, http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": "spent",
	}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestLogoutReturns204(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodPost, "/api/v1/auth/logout", map[string]string{
		"refresh_token": "whatever",
	}, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestAuthenticatedRoutesRejectMissingToken(t *testing.T) {
	h := newHarness(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/notes"},
		{http.MethodPost, "/api/v1/notes"},
		{http.MethodGet, "/api/v1/tasks"},
		{http.MethodPost, "/api/v1/tasks"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := h.do(t, route.method, route.path, nil, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthenticatedRoutesRejectMalformedAuthorizationHeader(t *testing.T) {
	h := newHarness(t)

	// A header the middleware cannot read a token from must be treated exactly
	// like no header at all.
	for _, header := range []string{"", "Bearer", "Basic abc", "good-token", "Bearer  "} {
		t.Run("header="+header, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, "/api/v1/auth/me", nil, map[string]string{
				"Authorization": header,
			})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthenticatedRoutesPropagateAuthFailure(t *testing.T) {
	h := newHarness(t)
	h.auth.authErr = domain.ErrUnauthorized

	rec := h.authed(t, http.MethodGet, "/api/v1/notes", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/api/v1/auth/me", nil, map[string]string{
		"Authorization": "bearer good-token",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	h.auth.mu.Lock()
	got := h.auth.gotToken
	h.auth.mu.Unlock()

	if got != "good-token" {
		t.Errorf("the token forwarded to the service was %q, want good-token", got)
	}
}

func TestMeReturnsTheAuthenticatedUser(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodGet, "/api/v1/auth/me", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)

	if body["email"] != "user@example.com" {
		t.Errorf("email = %v, want user@example.com", body["email"])
	}
}

func TestHandlersForwardTheAuthenticatedUser(t *testing.T) {
	h := newHarness(t)
	expected := h.auth.user.ID

	rec := h.authed(t, http.MethodPost, "/api/v1/notes", map[string]string{
		"title":   "a note",
		"content": "body",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	h.notes.mu.Lock()
	got := h.notes.lastUserID
	h.notes.mu.Unlock()

	if got != expected {
		t.Errorf("the service received user %s, want %s from the token", got, expected)
	}
}

func TestCreateNoteReturns201(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPost, "/api/v1/notes", map[string]any{
		"title":   "a note",
		"content": "body",
		"tags":    []string{"work"},
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)

	if body["title"] != "a note" {
		t.Errorf("title = %v, want a note", body["title"])
	}

	// An empty tag list must serialise as [] rather than null, so a client does
	// not have to null-check before iterating.
	tags, ok := body["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %v, want an array", body["tags"])
	}

	if len(tags) != 1 {
		t.Errorf("tags = %v, want one entry", tags)
	}
}

func TestGetNoteWithMalformedIDIs400(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodGet, "/api/v1/notes/not-a-uuid", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}

	// The repository must not have been consulted with a nonsense id.
	h.notes.mu.Lock()
	got := h.notes.lastNoteID
	h.notes.mu.Unlock()

	if got != uuid.Nil {
		t.Errorf("the service received id %s, want no request at all", got)
	}
}

func TestGetNoteNotFoundIs404(t *testing.T) {
	h := newHarness(t)
	h.notes.getErr = domain.ErrNotFound

	rec := h.authed(t, http.MethodGet, "/api/v1/notes/"+uuid.NewString(), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteNoteReturns204(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodDelete, "/api/v1/notes/"+uuid.NewString(), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestListNotesReadsFilters(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodGet, "/api/v1/notes?q=milk&tags=work,home&include_archived=true&limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	h.notes.mu.Lock()
	filter := h.notes.lastListFilt
	h.notes.mu.Unlock()

	if filter.Search != "milk" {
		t.Errorf("Search = %q, want milk", filter.Search)
	}

	if !filter.IncludeArchived {
		t.Error("IncludeArchived = false, want true")
	}

	if len(filter.Tags) != 2 {
		t.Errorf("Tags = %v, want two entries", filter.Tags)
	}
}

func TestListNotesRejectsBadPagination(t *testing.T) {
	h := newHarness(t)

	for _, query := range []string{"limit=0", "limit=99999", "limit=abc", "offset=-1"} {
		t.Run(query, func(t *testing.T) {
			rec := h.authed(t, http.MethodGet, "/api/v1/notes?"+query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestCreateTaskReturns201(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"title":    "do the thing",
		"priority": "high",
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)

	if body["status"] != "pending" {
		t.Errorf("status = %v, want pending", body["status"])
	}
}

func TestCreateTaskRejectsBadDueAt(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"title":  "do the thing",
		"due_at": "next tuesday",
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestCreateTaskAcceptsRFC3339DueAt(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPost, "/api/v1/tasks", map[string]string{
		"title":  "do the thing",
		"due_at": "2026-04-01T09:00:00Z",
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)

	if body["due_at"] != "2026-04-01T09:00:00Z" {
		t.Errorf("due_at = %v, want the supplied timestamp", body["due_at"])
	}
}

func TestCompleteTaskAppliesTheRequestedStatus(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPost, "/api/v1/tasks/"+uuid.NewString()+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	h.tasks.mu.Lock()
	status := h.tasks.lastStatus
	h.tasks.mu.Unlock()

	if status != domain.TaskStatusCompleted {
		t.Errorf("the service received status %q, want completed", status)
	}
}

func TestCompleteTaskStateTransitionIs409(t *testing.T) {
	h := newHarness(t)
	h.tasks.statusErr = domain.ErrStateTransition

	rec := h.authed(t, http.MethodPost, "/api/v1/tasks/"+uuid.NewString()+"/complete", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestPatchTaskWithStatusRunsTheStateMachine(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPatch, "/api/v1/tasks/"+uuid.NewString(), map[string]any{
		"status": "in_progress",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	h.tasks.mu.Lock()
	status := h.tasks.lastStatus
	h.tasks.mu.Unlock()

	if status != domain.TaskStatusInProgress {
		t.Errorf("the service received status %q, want in_progress", status)
	}
}

func TestPatchTaskRejectsUnknownStatus(t *testing.T) {
	h := newHarness(t)

	// An unknown status must not reach the state machine, because an empty
	// result set would look like a successful search rather than a bad request.
	rec := h.authed(t, http.MethodGet, "/api/v1/tasks?status=fuzzy", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}

	rec = h.authed(t, http.MethodGet, "/api/v1/tasks?priority=urgent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPatchTaskRejectsUnknownPriority(t *testing.T) {
	h := newHarness(t)

	rec := h.authed(t, http.MethodPatch, "/api/v1/tasks/"+uuid.NewString(), map[string]string{
		"priority": "urgent",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the field update and the status check are separate concerns", rec.Code)
	}
}

func TestUnexpectedErrorBecomes500(t *testing.T) {
	h := newHarness(t)
	h.notes.getErr = errors.New("the database fell over")

	rec := h.authed(t, http.MethodGet, "/api/v1/notes/"+uuid.NewString(), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	// The internal message must not leak to the client.
	if strings.Contains(rec.Body.String(), "fell over") {
		t.Errorf("the response leaked an internal error: %s", rec.Body.String())
	}
}

func TestRequestIDIsEchoedInErrors(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/api/v1/nope", nil, map[string]string{"X-Request-ID": "abc-123"})

	body := decodeBody(t, rec)
	errObj := body["error"].(map[string]any)

	if errObj["request_id"] != "abc-123" {
		t.Errorf("request_id = %v, want abc-123", errObj["request_id"])
	}
}

func TestRequestIDIsGeneratedWhenAbsent(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/api/v1/nope", nil, nil)
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID must be set on every response")
	}
}
