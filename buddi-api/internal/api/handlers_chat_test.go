package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/auth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// stubChat emits a fixed sequence of events, so the handler can be exercised
// without a model.
type stubChat struct {
	events []chat.Event
	err    error

	turns []chat.Turn
	user  uuid.UUID

	conversations []domain.Conversation
	messages      []domain.Message
}

func (s *stubChat) Turn(
	_ context.Context,
	userID uuid.UUID,
	turn chat.Turn,
	emit chat.Emitter,
) (*chat.Result, error) {
	s.turns = append(s.turns, turn)
	s.user = userID

	for _, event := range s.events {
		if err := emit(event); err != nil {
			return nil, err
		}
	}

	if s.err != nil {
		return nil, s.err
	}

	return &chat.Result{}, nil
}

func (s *stubChat) ListConversations(
	_ context.Context, _ uuid.UUID, _, _ int,
) ([]domain.Conversation, int64, error) {
	return s.conversations, int64(len(s.conversations)), nil
}

func (s *stubChat) GetConversation(
	_ context.Context, _ uuid.UUID, _ uuid.UUID,
) (*domain.Conversation, []domain.Message, error) {
	if len(s.conversations) == 0 {
		return nil, nil, domain.ErrNotFound
	}

	return &s.conversations[0], s.messages, nil
}

func (s *stubChat) DeleteConversation(
	_ context.Context, _ uuid.UUID, _ uuid.UUID,
) error {
	return s.err
}

// stubAuthenticator lets every request through as one user.
type stubAuthenticator struct {
	user *domain.User
	err  error
}

func (s stubAuthenticator) Register(
	context.Context, auth.RegisterInput,
) (*auth.Session, error) {
	return nil, errors.New("unused")
}

func (s stubAuthenticator) Login(
	context.Context, string, string, auth.TokenMeta,
) (*auth.Session, error) {
	return nil, errors.New("unused")
}

func (s stubAuthenticator) Refresh(
	context.Context, string, auth.TokenMeta,
) (*auth.Session, error) {
	return nil, errors.New("unused")
}

func (s stubAuthenticator) Logout(context.Context, string) error { return nil }

func (s stubAuthenticator) Authenticate(_ context.Context, _ string) (*domain.User, error) {
	if s.err != nil {
		return nil, s.err
	}

	return s.user, nil
}

var chatTestUser = &domain.User{ID: uuid.New(), Email: "chat@example.com"}

func chatTestAPI(t *testing.T, chatSvc ChatService) http.Handler {
	t.Helper()

	return newTestAPI(t, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:   stubAuthenticator{user: chatTestUser},
		Chat:   chatSvc,
	}).Handler()
}

func postChat(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// sseEvent is one parsed event from a recorded stream.
type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()

	var events []sseEvent

	var name string
	var data strings.Builder

	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "" && name != "":
			payload := map[string]any{}
			if err := json.Unmarshal([]byte(data.String()), &payload); err != nil {
				t.Fatalf("event %q carried invalid JSON %q: %v", name, data.String(), err)
			}

			events = append(events, sseEvent{name: name, data: payload})

			name = ""
			data.Reset()
		}
	}

	return events
}

func TestStreamTurnEmitsServerSentEvents(t *testing.T) {
	svc := &stubChat{events: []chat.Event{
		{
			Type: chat.EventStart, ConversationID: uuid.New(),
			MessageID: uuid.New(), QuestionID: uuid.New(),
		},
		{Type: chat.EventDelta, MessageID: uuid.MustParse(
			"00000000-0000-0000-0000-000000000002"), Delta: "Hello"},
		{Type: chat.EventDelta, Delta: " there"},
		{Type: chat.EventDone, Content: "Hello there"},
	}}

	rec := postChat(t, chatTestAPI(t, svc), `{"message":"hi","mode":"chat"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// A proxy that buffers this response would undo the whole point of the endpoint.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}

	if events[0].name != "start" || events[len(events)-1].name != "done" {
		t.Errorf("event order = %q..%q, want start..done", events[0].name, events[len(events)-1].name)
	}

	if events[0].data["question_id"] == nil {
		t.Error("start did not report the user's message, so it cannot be edited")
	}

	// Deltas are the point: they must be separate events so a client can append
	// them as they arrive rather than waiting for the whole reply.
	if events[1].data["delta"] != "Hello" || events[2].data["delta"] != " there" {
		t.Errorf("deltas = %v, %v", events[1].data["delta"], events[2].data["delta"])
	}
}

// Reasoning has to arrive as its own event, or a client cannot show it separately
// from the answer.
func TestStreamTurnSeparatesReasoning(t *testing.T) {
	svc := &stubChat{events: []chat.Event{
		{Type: chat.EventReasoning, Reasoning: "weighing ", ReasoningAvailable: true},
		{Type: chat.EventReasoning, Reasoning: "options", ReasoningAvailable: true},
		{Type: chat.EventDelta, Delta: "answer"},
		{Type: chat.EventDone, ReasoningComplete: "weighing options"},
	}}

	rec := postChat(t, chatTestAPI(t, svc), `{"message":"hi","mode":"chat"}`)

	var reasoning string

	for _, event := range parseSSE(t, rec.Body.String()) {
		if event.name == "reasoning" {
			reasoning += event.data["reasoning"].(string)

			if event.data["reasoning_available"] != true {
				t.Error("a reasoning event did not say reasoning was available")
			}
		}
	}

	if reasoning != "weighing options" {
		t.Errorf("reasoning = %q, want %q", reasoning, "weighing options")
	}
}

// A failure after the headers are sent cannot change the status code, so it has to
// arrive as an event carrying the same shape every other endpoint uses.
func TestStreamTurnReportsFailureAsAnEvent(t *testing.T) {
	svc := &stubChat{events: []chat.Event{{Type: chat.EventStart}}, err: domain.Invalid("bad", nil)}

	rec := postChat(t, chatTestAPI(t, svc), `{"message":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: once streaming starts the status is spent", rec.Code)
	}

	events := parseSSE(t, rec.Body.String())

	var failure *sseEvent

	for i := range events {
		if events[i].name == "error" {
			failure = &events[i]
		}
	}

	if failure == nil {
		t.Fatalf("no error event was sent: %+v", events)
	}

	body, ok := failure.data["error"].(map[string]any)
	if !ok {
		t.Fatalf("error event does not use the standard error envelope: %+v", failure.data)
	}

	if body["code"] != "validation_failed" {
		t.Errorf("code = %v, want validation_failed", body["code"])
	}
}

func TestStreamTurnRejectsAnEmptyMessage(t *testing.T) {
	svc := &stubChat{}

	rec := postChat(t, chatTestAPI(t, svc), `{"message":"   "}`)

	// Validation happens before the stream opens, so this is still a plain JSON error.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}

	if got := rec.Header().Get("Content-Type"); strings.Contains(got, "event-stream") {
		t.Error("a rejected request opened a stream instead of returning an error")
	}
}

func TestStreamTurnPassesTheTurnThrough(t *testing.T) {
	svc := &stubChat{}

	conversation := uuid.New()
	parent := uuid.New()

	body := `{"message":"changed my mind","conversation_id":"` + conversation.String() +
		`","parent_id":"` + parent.String() + `","mode":"plan","edit":true}`

	postChat(t, chatTestAPI(t, svc), body)

	if len(svc.turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(svc.turns))
	}

	turn := svc.turns[0]

	if turn.ConversationID == nil || *turn.ConversationID != conversation {
		t.Errorf("ConversationID = %v, want %v", turn.ConversationID, conversation)
	}

	if turn.ParentID == nil || *turn.ParentID != parent {
		t.Errorf("ParentID = %v, want %v", turn.ParentID, parent)
	}

	if turn.Mode != chat.ModePlan {
		t.Errorf("Mode = %q, want plan", turn.Mode)
	}

	if !turn.Edit {
		t.Error("Edit was dropped")
	}

	if svc.user != chatTestUser.ID {
		t.Errorf("turn ran as %v, want the authenticated user", svc.user)
	}
}

func TestStreamTurnRequiresAuthentication(t *testing.T) {
	handler := chatTestAPI(t, &stubChat{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/messages", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestListConversations(t *testing.T) {
	title := "buy milk"
	head := uuid.New()

	svc := &stubChat{conversations: []domain.Conversation{{
		ID:            uuid.New(),
		UserID:        chatTestUser.ID,
		Title:         &title,
		HeadMessageID: &head,
	}}}

	handler := chatTestAPI(t, svc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations?limit=5", nil)
	req.Header.Set("Authorization", "Bearer test")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response struct {
		Data  []conversationResponse `json:"data"`
		Total int64                  `json:"total"`
		Limit int                    `json:"limit"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(response.Data) != 1 || response.Data[0].Title != "buy milk" {
		t.Fatalf("data = %+v", response.Data)
	}

	if response.Limit != 5 {
		t.Errorf("limit = %d, want 5", response.Limit)
	}
}

func TestGetConversationReturnsMessages(t *testing.T) {
	title := "buy milk"
	reasoning := "the request named a deadline"

	svc := &stubChat{
		conversations: []domain.Conversation{{ID: uuid.New(), UserID: chatTestUser.ID, Title: &title}},
		messages: []domain.Message{
			{ID: uuid.New(), Role: domain.MessageRoleUser, Content: "buy milk"},
			{
				ID: uuid.New(), Role: domain.MessageRoleAssistant,
				Content: "Planned: buy milk.", Reasoning: &reasoning,
			},
		},
	}

	id := svc.conversations[0].ID

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations/"+id.String(), nil)
	req.Header.Set("Authorization", "Bearer test")

	rec := httptest.NewRecorder()
	chatTestAPI(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var response struct {
		Conversation conversationResponse `json:"conversation"`
		Messages     []messageResponse    `json:"messages"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(response.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(response.Messages))
	}

	// Reasoning is carried through so the client can show it without another request.
	if response.Messages[1].Reasoning == nil || *response.Messages[1].Reasoning != reasoning {
		t.Errorf("reasoning = %v, want it preserved", response.Messages[1].Reasoning)
	}
}

func TestGetConversationRejectsAMalformedID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations/not-a-uuid", nil)
	req.Header.Set("Authorization", "Bearer test")

	rec := httptest.NewRecorder()
	chatTestAPI(t, &stubChat{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

var _ = agent.Run{}
