// Package chat turns a thread of messages into streamed replies.
//
// The agent surface this replaces answered exactly one question per run: a goal
// went in, a plan came out, and if the plan was wrong there was nowhere to correct
// it. This package adds the transcript that makes a second attempt possible, and it
// streams what the model produces because on a CPU runtime a reply takes long
// enough that silence and progress look identical.
//
// The model is still treated as untrusted. Whatever it streams is shown as it
// arrives, but nothing it produced is treated as a decision: a plan is validated
// before it becomes an approval, and a reply is the model's words rather than the
// system's.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// MaxChatNumPredict bounds a conversational reply.
//
// Bounded rather than generous because the cost is linear and the value is not: a
// reply longer than a few sentences is not more useful in a transcript, and on this
// runtime every extra token is a tenth of a second the user is waiting through.
const MaxChatNumPredict = 400

// Mode is how a turn is handled.
type Mode string

const (
	// ModeAuto lets the service decide from the message itself.
	ModeAuto Mode = ""

	// ModePlan forces planning, producing a task for the user to approve.
	ModePlan Mode = "plan"

	// ModeChat forces a conversational reply, with no task and no approval.
	ModeChat Mode = "chat"
)

// IsValid reports whether the mode is one the service defines. ModeAuto is valid
// because it is the default, not because it is a caller choice.
func (m Mode) IsValid() bool {
	switch m {
	case ModeAuto, ModePlan, ModeChat:
		return true
	default:
		return false
	}
}

// EventType names what an Event reports.
type EventType string

const (
	// EventStart opens a turn. It carries the assistant message every later event
	// belongs to, so a client can create the bubble before any text exists.
	EventStart EventType = "start"

	// EventReasoning carries one piece of the model's reasoning.
	EventReasoning EventType = "reasoning"

	// EventDelta carries one piece of the answer.
	EventDelta EventType = "delta"

	// EventPlan reports the validated plan once generation has finished.
	EventPlan EventType = "plan"

	// EventDone closes a successful turn.
	EventDone EventType = "done"
)

// Event is one thing that happened during a turn.
//
// The set is closed on purpose. A client switches on it, so a type added later is
// something every existing client has to learn about, and an event of unknown type
// cannot be rendered.
type Event struct {
	Type EventType

	ConversationID uuid.UUID
	MessageID      uuid.UUID

	// Mode is how this turn was resolved, on the start event only.
	//
	// It is sent first so a client can render accordingly: a planned turn streams
	// machine JSON that is not meant to be read, and a conversational one streams the
	// user's answer. Without it the client has to show both as raw text.
	Mode Mode

	// QuestionID is the user's own message this turn answers.
	//
	// Sent on start because a client needs it to offer "edit and resend" on the right
	// message, and by the time the reply is streaming there is no other way to tell
	// which turn is being corrected.
	QuestionID uuid.UUID

	// Delta is newly generated answer text.
	Delta string

	// Reasoning is newly generated reasoning text.
	Reasoning string

	// ReasoningAvailable reports whether this model exposes reasoning at all.
	//
	// Sent once at the start so a client can say "this model does not explain its
	// reasoning" rather than rendering an empty panel that never fills.
	ReasoningAvailable bool

	// ReasoningComplete carries the reasoning as a whole, at the end. A client that
	// wants to store or redisplay it should not have to reassemble it from deltas,
	// which also means it cannot get out of step with them.
	ReasoningComplete string

	// Run points at the run a planned turn produced.
	Run *RunRef

	// Fallback reports that the plan shown is deterministic rather than the model's.
	Fallback bool

	// Content is the finished message text.
	Content string
}

// RunRef points at the run a planned turn produced.
type RunRef struct {
	ID     uuid.UUID
	Status string
}

// Result is what a turn produced, for callers that want the outcome after the fact
// rather than as events.
type Result struct {
	Conversation *domain.Conversation
	Message      *domain.Message
	Run          *RunRef
	Fallback     bool
}

// Emitter receives turn events. Returning an error stops the turn.
//
// An emitter error aborts rather than falling back to a saved answer, because the
// only reasons to stop reading are that the client disconnected or that it already
// has what it wanted. Neither is improved by generating more.
type Emitter func(Event) error

// Turn is one request to speak.
type Turn struct {
	// Input is the user's message.
	Input string

	// ConversationID is the thread to append to. Nil starts a new conversation.
	ConversationID *uuid.UUID

	// ParentID is the message this turn answers or revises. Nil means the
	// conversation's current head.
	ParentID *uuid.UUID

	// Mode forces how the turn is handled. ModeAuto decides from the message.
	Mode Mode

	// Edit makes this turn revise ParentID instead of appending.
	Edit bool
}

// Streamer produces free-form text for a conversational turn.
type Streamer interface {
	Stream(ctx context.Context, req StreamRequest, onDelta func(Delta) error) (StreamResult, error)
}

// StreamRequest is one conversational generation.
type StreamRequest struct {
	Prompt     string
	NumPredict int
}

// Delta is one piece of a streamed generation.
type Delta struct {
	Text      string
	Reasoning string
}

// StreamResult is a completed conversational generation.
type StreamResult struct {
	Text      string
	Reasoning string
	Model     string
	Elapsed   time.Duration
}

// Planner produces a validated plan for a goal.
type Planner interface {
	Plan(ctx context.Context, userID uuid.UUID, goal string) (*planner.Outcome, error)
}

// RunRecorder turns an already-produced plan into a run awaiting approval.
//
// It takes an outcome rather than a goal because the chat surface streams the plan
// to the user as it is generated. Asking the agent to plan again would pay for the
// same planning call twice, which on a CPU runtime is the whole cost of a turn.
type RunRecorder interface {
	PlanFromOutcome(
		ctx context.Context,
		userID uuid.UUID,
		goal string,
		outcome *planner.Outcome,
	) (RunRecord, error)
}

// RunRecord is the part of a recorded run a chat client needs.
type RunRecord struct {
	ID     uuid.UUID
	Status string
}

// Service runs chat turns.
type Service struct {
	conversations domain.ConversationRepository
	messages      domain.MessageRepository
	planner       Planner
	runs          RunRecorder
	streamer      Streamer

	now func() time.Time

	// historyTokens bounds the transcript offered to the model. See
	// Options.HistoryTokens for why it is bounded rather than unlimited.
	historyTokens int
}

// Options configures a Service.
type Options struct {
	// HistoryTokens bounds how much transcript is sent with a conversational turn.
	//
	// It exists because the transcript grows without limit and the runtime's context
	// window does not. Without a budget the oldest turns are dropped by the runtime,
	// silently, and the failure looks like the model forgetting. Zero uses
	// DefaultHistoryTokens.
	HistoryTokens int

	// Clock returns the current time.
	Clock func() time.Time
}

// NewService builds a chat service.
func NewService(
	conversations domain.ConversationRepository,
	messages domain.MessageRepository,
	pl Planner,
	runs RunRecorder,
	streamer Streamer,
	opts Options,
) (*Service, error) {
	if conversations == nil {
		return nil, errors.New("chat: conversations repository is required")
	}

	if messages == nil {
		return nil, errors.New("chat: messages repository is required")
	}

	if pl == nil {
		return nil, errors.New("chat: planner is required")
	}

	if runs == nil {
		return nil, errors.New("chat: run recorder is required")
	}

	if streamer == nil {
		return nil, errors.New("chat: streamer is required")
	}

	now := opts.Clock
	if now == nil {
		now = time.Now
	}

	historyTokens := opts.HistoryTokens
	if historyTokens <= 0 {
		historyTokens = DefaultHistoryTokens
	}

	return &Service{
		conversations: conversations,
		messages:      messages,
		planner:       pl,
		runs:          runs,
		streamer:      streamer,
		now:           now,
		historyTokens: historyTokens,
	}, nil
}

// ValidateTurn reports whether a turn is well formed enough to start.
//
// It is exported so a caller can check before committing to a streaming response:
// once the response status is sent, a rejection can only travel as an event, which
// a client cannot map back onto a request it should fix. Turn calls it too, so the
// rules exist once rather than twice.
func ValidateTurn(turn Turn) error {
	if strings.TrimSpace(turn.Input) == "" {
		return domain.Invalid("message is required",
			map[string]string{"message": "must not be empty"})
	}

	if !turn.Mode.IsValid() {
		return domain.Invalid("mode is not recognised",
			map[string]string{"mode": "must be plan or chat"})
	}

	// An edit with nothing to edit is refused rather than treated as a new message:
	// appending would give the user a second copy of a question instead of the
	// correction they asked for.
	if turn.Edit && turn.ParentID == nil {
		return domain.Invalid("there is no message to edit",
			map[string]string{"parent_id": "must be set when editing"})
	}

	return nil
}

// Turn runs one turn, emitting events as it goes.
//
// The user's message is recorded before the model is called, so an interrupted
// generation still leaves the question on the record. The assistant's message is
// created before generation too, so deltas have something to attach to, and
// completed afterwards, so an abandoned turn keeps whatever text it produced.
func (s *Service) Turn(ctx context.Context, userID uuid.UUID, turn Turn, emit Emitter) (*Result, error) {
	if emit == nil {
		emit = func(Event) error { return nil }
	}

	if err := ValidateTurn(turn); err != nil {
		return nil, err
	}

	content := strings.TrimSpace(turn.Input)

	conversation, err := s.resolve(ctx, userID, turn.ConversationID)
	if err != nil {
		return nil, err
	}

	question, err := s.recordQuestion(ctx, userID, conversation, turn, content)
	if err != nil {
		return nil, err
	}

	// Resolved before anything is emitted, so the first event can tell the client
	// which path was taken. A client that learns this only at the end has to render
	// the plan's raw JSON while it streams, because it cannot know that prose is
	// coming.
	mode := turn.Mode
	if mode == ModeAuto {
		mode = Decide(content)
	}

	answer, err := domain.NewPendingMessage(
		conversation.ID, userID, &question.ID, s.now(),
	)
	if err != nil {
		return nil, err
	}

	if err := s.messages.Create(ctx, answer); err != nil {
		return nil, err
	}

	if err := emit(Event{
		Type:           EventStart,
		ConversationID: conversation.ID,
		MessageID:      answer.ID,
		QuestionID:     question.ID,
		Mode:           mode,
	}); err != nil {
		return nil, err
	}

	if mode == ModePlan {
		return s.planTurn(ctx, userID, conversation, question, answer, content, emit)
	}

	return s.replyTurn(ctx, userID, conversation, question, answer, content, emit)
}

// resolve loads the conversation this turn belongs to, or creates one.
func (s *Service) resolve(
	ctx context.Context,
	userID uuid.UUID,
	id *uuid.UUID,
) (*domain.Conversation, error) {
	if id != nil {
		// Scoped to the user, so another user's conversation id reports not found
		// rather than being served.
		return s.conversations.GetByID(ctx, userID, *id)
	}

	conversation, err := domain.NewConversation(userID, s.now())
	if err != nil {
		return nil, err
	}

	if err := s.conversations.Create(ctx, conversation); err != nil {
		return nil, err
	}

	return conversation, nil
}

// recordQuestion stores the user's message and moves the head onto it, so the next
// turn has something to answer.
func (s *Service) recordQuestion(
	ctx context.Context,
	userID uuid.UUID,
	conversation *domain.Conversation,
	turn Turn,
	content string,
) (*domain.Message, error) {
	parent := conversation.HeadMessageID

	if turn.ParentID != nil {
		parent = turn.ParentID
	}

	now := s.now()

	var (
		message *domain.Message
		err     error
	)

	switch {
	case turn.Edit:
		original, loadErr := s.messages.GetByID(ctx, userID, *parent)
		if loadErr != nil {
			return nil, loadErr
		}

		// The edit has to belong to this conversation, or a caller could move a
		// message between threads by naming a parent id from another one.
		if original.ConversationID != conversation.ID {
			return nil, domain.ErrNotFound
		}

		// Only a user's own message can be revised. Editing an assistant reply would
		// mean rewriting what the assistant said, which is not a correction the user
		// makes to their own request and would let a caller forge a transcript.
		if original.Role != domain.MessageRoleUser {
			return nil, domain.Invalid("only your own message can be edited",
				map[string]string{"parent_id": "must be a message you sent"})
		}

		message, err = original.AsEdit(content, now)

	default:
		message, err = domain.NewMessage(
			conversation.ID, userID, domain.MessageRoleUser, content, parent, now,
		)
	}

	if err != nil {
		return nil, err
	}

	if err := s.messages.Create(ctx, message); err != nil {
		return nil, err
	}

	// The first message names the thread. Later ones must not overwrite it: a
	// conversation called "buy milk" that became "buy oat milk" on the second turn
	// would rename itself out from under the user's own history.
	if conversation.Title == nil {
		conversation.SetTitle(domain.DeriveTitle(content))
	}

	conversation.HeadMessageID = &message.ID
	conversation.Touch(now)

	if err := s.conversations.Update(ctx, conversation); err != nil {
		return nil, err
	}

	return message, nil
}

// planTurn plans the request, streaming the plan as it is generated, then records a
// run so the user can approve it.
func (s *Service) planTurn(
	ctx context.Context,
	userID uuid.UUID,
	conversation *domain.Conversation,
	question, answer *domain.Message,
	content string,
	emit Emitter,
) (*Result, error) {
	outcome, err := s.plan(ctx, userID, content, answer, emit)
	if err != nil {
		return nil, err
	}

	record, err := s.runs.PlanFromOutcome(ctx, userID, content, outcome)
	if err != nil {
		return nil, err
	}

	ref := &RunRef{ID: record.ID, Status: record.Status}

	reply := summarisePlan(outcome.Plan, outcome.UsedFallback)

	if err := s.complete(ctx, conversation, answer, reply, outcome.Reasoning, record.ID); err != nil {
		return nil, err
	}

	if err := emit(Event{
		Type:               EventPlan,
		ConversationID:     conversation.ID,
		MessageID:          answer.ID,
		Run:                ref,
		Fallback:           outcome.UsedFallback,
		ReasoningComplete:  outcome.Reasoning,
		ReasoningAvailable: strings.TrimSpace(outcome.Reasoning) != "",
		Content:            reply,
	}); err != nil {
		return nil, err
	}

	if err := emit(Event{Type: EventDone, ConversationID: conversation.ID, MessageID: answer.ID,
		Run: ref, Content: reply}); err != nil {
		return nil, err
	}

	return &Result{
		Conversation: conversation,
		Message:      answer,
		Run:          ref,
		Fallback:     outcome.UsedFallback,
	}, nil
}

// plan runs the planner with an emitter wired to this turn's events.
//
// The emitter is per-call state on a planner that is shared across every request,
// so it is attached for the duration of this call and cleared immediately after.
// Leaving it set would leak one user's turn into another user's stream.
//
// A planner that cannot stream is still used, unstreamed: the turn then shows no
// progressive text and reports the plan when it is ready, which is slower to look
// at but not wrong.
func (s *Service) plan(
	ctx context.Context,
	userID uuid.UUID,
	content string,
	answer *domain.Message,
	emit Emitter,
) (*planner.Outcome, error) {
	service, ok := s.planner.(*planner.Service)
	if !ok {
		return s.planner.Plan(ctx, userID, content)
	}

	service.WithEmitter(func(delta planner.Delta) error {
		if delta.Reasoning != "" {
			if err := emit(Event{
				Type:      EventReasoning,
				MessageID: answer.ID,
				Reasoning: delta.Reasoning,
				// Set on every reasoning delta rather than once at the start: whether
				// a model reasons is not known until it does, and a client that
				// committed to "no reasoning" at the start would have to be corrected
				// mid-stream.
				ReasoningAvailable: true,
			}); err != nil {
				return err
			}
		}

		if delta.Text == "" {
			return nil
		}

		return emit(Event{
			Type:      EventDelta,
			MessageID: answer.ID,
			Delta:     delta.Text,
		})
	})

	defer service.WithEmitter(nil)

	return service.Plan(ctx, userID, content)
}

// replyTurn answers conversationally, with no plan and no approval.
func (s *Service) replyTurn(
	ctx context.Context,
	userID uuid.UUID,
	conversation *domain.Conversation,
	question, answer *domain.Message,
	content string,
	emit Emitter,
) (*Result, error) {
	history, err := s.history(ctx, userID, conversation.ID, question.ID)
	if err != nil {
		return nil, err
	}

	res, err := s.streamer.Stream(ctx, StreamRequest{
		Prompt:     replyPrompt(history, content, s.historyTokens),
		NumPredict: MaxChatNumPredict,
	}, func(delta Delta) error {
		if delta.Reasoning != "" {
			if err := emit(Event{
				Type:               EventReasoning,
				MessageID:          answer.ID,
				Reasoning:          delta.Reasoning,
				ReasoningAvailable: true,
			}); err != nil {
				return err
			}
		}

		if delta.Text == "" {
			return nil
		}

		return emit(Event{
			Type:      EventDelta,
			MessageID: answer.ID,
			Delta:     delta.Text,
		})
	})
	if err != nil {
		return nil, err
	}

	reply := strings.TrimSpace(res.Text)
	if reply == "" {
		// A turn that produced nothing still has to produce a message, or the
		// transcript shows a question that was never answered and the client is left
		// rendering a spinner forever.
		reply = "I could not produce a reply for that."
	}

	if err := s.complete(ctx, conversation, answer, reply, res.Reasoning, uuid.Nil); err != nil {
		return nil, err
	}

	if err := emit(Event{
		Type:               EventDone,
		MessageID:          answer.ID,
		ReasoningComplete:  res.Reasoning,
		ReasoningAvailable: strings.TrimSpace(res.Reasoning) != "",
		Content:            reply,
	}); err != nil {
		return nil, err
	}

	return &Result{
		Conversation: conversation,
		Message:      answer,
	}, nil
}

// complete writes the finished message and moves the conversation head onto it.
func (s *Service) complete(
	ctx context.Context,
	conversation *domain.Conversation,
	answer *domain.Message,
	content string,
	reasoning string,
	runID uuid.UUID,
) error {
	if err := answer.Complete(content); err != nil {
		return err
	}

	answer.WithReasoning(reasoning)
	answer.WithRun(runID)

	if err := s.messages.Update(ctx, answer); err != nil {
		return err
	}

	conversation.HeadMessageID = &answer.ID
	conversation.Touch(s.now())

	return s.conversations.Update(ctx, conversation)
}

// history returns the turns leading up to the question being answered.
func (s *Service) history(
	ctx context.Context,
	userID uuid.UUID,
	conversationID uuid.UUID,
	exclude uuid.UUID,
) ([]TurnView, error) {
	messages, err := s.messages.ListActivePath(ctx, userID, conversationID)
	if err != nil {
		return nil, err
	}

	views := make([]TurnView, 0, len(messages))

	for _, message := range messages {
		// The question being answered is appended by the prompt separately, so
		// including it here would say it twice.
		if message.ID == exclude {
			continue
		}

		views = append(views, TurnView{Role: message.Role, Content: message.Content})
	}

	return views, nil
}

// TurnView is one prior turn as it is shown to the model.
type TurnView struct {
	Role    domain.MessageRole
	Content string
}

// ListConversations returns the user's threads, most recently touched first.
func (s *Service) ListConversations(
	ctx context.Context,
	userID uuid.UUID,
	limit int,
	offset int,
) ([]domain.Conversation, int64, error) {
	total, err := s.conversations.Count(ctx, userID)
	if err != nil {
		return nil, 0, err
	}

	conversations, err := s.conversations.List(ctx, userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}

	return conversations, total, nil
}

// GetConversation returns one thread with the messages on its current branch.
//
// Superseded messages are excluded, because a thread that shows both a question and
// its correction reads as two questions. They remain stored and are still reachable
// by id.
func (s *Service) GetConversation(
	ctx context.Context,
	userID uuid.UUID,
	id uuid.UUID,
) (*domain.Conversation, []domain.Message, error) {
	conversation, err := s.conversations.GetByID(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}

	messages, err := s.messages.ListActivePath(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}

	return conversation, messages, nil
}

// DeleteConversation removes a thread. Its messages cascade in the schema.
func (s *Service) DeleteConversation(ctx context.Context, userID uuid.UUID, id uuid.UUID) error {
	return s.conversations.Delete(ctx, userID, id)
}

var _ = fmt.Sprintf
