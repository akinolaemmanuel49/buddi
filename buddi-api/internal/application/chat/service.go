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

	// EventClarification reports that the turn stopped to ask a question instead of
	// proposing anything.
	//
	// It is a separate event rather than a plan with an empty body because the two
	// need different things from a client: a plan wants an approval card, and a
	// question wants a reply box. Sending one as the other produces an approval card
	// with nothing to approve, or a plan that silently does nothing.
	EventClarification EventType = "clarification"

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

	// Clarification is the question asked instead of a plan, on a clarification event.
	Clarification *Clarification

	// Content is the finished message text.
	Content string
}

// Clarification is a turn that stopped to ask rather than propose.
//
// It carries the question as well as the request it is about, so a client can render
// the question prominently while still being able to show what it was a question
// about after the conversation has moved on.
type Clarification struct {
	// Question is what to put to the user.
	Question string

	// Request is the original request the question is about.
	//
	// Sent so a client can say "about which request?" in a conversation with more than
	// one thing in it, and because it is what the next turn is re-planned against — a
	// reply of "Tuesday at 4pm" names no dentist without it.
	Request string
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

	// Clarification is set when the turn asked a question instead of proposing
	// anything. It is nil on every other turn, so a caller that ignores it is
	// unaffected.
	Clarification *Clarification
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

	// TimeZone is the IANA zone the user is in, or empty for UTC.
	//
	// It is taken from the client rather than configured, because it is a property of
	// the person using the browser and not of the deployment. Getting it wrong is
	// visible immediately: a 3pm appointment is written to the calendar as 4pm for
	// anyone east of Greenwich, and the plan's weekday check rejects the right date
	// while accepting the wrong one.
	TimeZone string
}

// zone resolves the turn's time zone, falling back to UTC.
//
// An unknown zone name is not an error. It comes from a browser, so the failure mode is
// a renamed zone in some future tzdata release, and refusing the turn would stop the
// user doing anything at all. Falling back to UTC degrades to the old behaviour, which
// is wrong by an hour rather than unusable.
func (t Turn) zone() *time.Location {
	if strings.TrimSpace(t.TimeZone) == "" {
		return time.UTC
	}

	loc, err := time.LoadLocation(strings.TrimSpace(t.TimeZone))
	if err != nil {
		return time.UTC
	}

	return loc
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
	PlanIn(ctx context.Context, userID uuid.UUID, goal string, loc *time.Location) (*planner.Outcome, error)
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
		return s.planTurn(ctx, userID, conversation, question, answer, content, turn.zone(), emit)
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
//
// A turn that comes back as a clarification never records a run. There is nothing to
// approve, and a run awaiting approval for a question would leave a card on screen
// with no action behind it.
func (s *Service) planTurn(
	ctx context.Context,
	userID uuid.UUID,
	conversation *domain.Conversation,
	question, answer *domain.Message,
	content string,
	loc *time.Location,
	emit Emitter,
) (*Result, error) {
	// A reply to a pending question is planned against the request the question was
	// about, not against the reply alone.
	request, err := s.resolvePendingRequest(ctx, userID, conversation, question, content)
	if err != nil {
		return nil, err
	}

	outcome, err := s.plan(ctx, userID, request, answer, loc, emit)
	if err != nil {
		return nil, err
	}

	if outcome.Plan != nil && outcome.Plan.Intent == domain.PlanIntentClarification {
		return s.clarifyTurn(ctx, conversation, answer, outcome, request, emit)
	}

	record, err := s.runs.PlanFromOutcome(ctx, userID, request, outcome)
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

// clarifyTurn records a question instead of a plan.
//
// The message content is the question, so a client that renders nothing but text — a
// transcript reloaded later, an export — still shows what was asked. The structured
// form is on the event for a client that can style it.
func (s *Service) clarifyTurn(
	ctx context.Context,
	conversation *domain.Conversation,
	answer *domain.Message,
	outcome *planner.Outcome,
	request string,
	emit Emitter,
) (*Result, error) {
	question := strings.TrimSpace(outcome.Plan.Question)

	if question == "" {
		// parsePlan rejects a clarification with no question, so this cannot be
		// reached through the planner. It is here because the plan may have come from
		// a Planner implementation this package does not control.
		return nil, errors.New("chat: the planner returned a clarification with no question")
	}

	if err := s.complete(ctx, conversation, answer, question, outcome.Reasoning, uuid.Nil); err != nil {
		return nil, err
	}

	// The request is stored so the next reply can be re-planned with the subject
	// attached. "Tuesday at 4pm" on its own names no dentist.
	answer.ClarificationRequest = &request

	if err := s.messages.Update(ctx, answer); err != nil {
		return nil, err
	}

	clarification := &Clarification{Question: question, Request: request}

	if err := emit(Event{
		Type:               EventClarification,
		ConversationID:     conversation.ID,
		MessageID:          answer.ID,
		Clarification:      clarification,
		ReasoningComplete:  outcome.Reasoning,
		ReasoningAvailable: strings.TrimSpace(outcome.Reasoning) != "",
		Content:            question,
	}); err != nil {
		return nil, err
	}

	if err := emit(Event{
		Type:           EventDone,
		ConversationID: conversation.ID,
		MessageID:      answer.ID,
		Clarification:  clarification,
		Content:        question,
	}); err != nil {
		return nil, err
	}

	return &Result{
		Conversation:  conversation,
		Message:       answer,
		Fallback:      outcome.UsedFallback,
		Clarification: clarification,
	}, nil
}

// resolvePendingRequest returns what this turn should actually be planned from.
//
// When the message being answered is a pending clarification, that is the original
// request followed by the user's reply. Planning from the reply alone loses the
// subject: "Tuesday at 4pm" is answerable only alongside "I need to see the dentist".
//
// Otherwise the reply is planned from itself.
func (s *Service) resolvePendingRequest(
	ctx context.Context,
	userID uuid.UUID,
	conversation *domain.Conversation,
	question *domain.Message,
	content string,
) (string, error) {
	if question.ParentID == nil {
		return content, nil
	}

	parent, err := s.messages.GetByID(ctx, userID, *question.ParentID)
	if err != nil {
		// The parent was read a moment ago to record this message, so a miss here is
		// not a normal condition and treating it as one would silently plan from the
		// wrong text.
		return "", err
	}

	if parent.ClarificationRequest == nil || strings.TrimSpace(*parent.ClarificationRequest) == "" {
		return content, nil
	}

	pending := strings.TrimSpace(*parent.ClarificationRequest)

	// Clear the marker now that it has been answered.
	//
	// Leaving it set would keep the thread looking like it was waiting for a reply to a
	// question already answered, so a reloaded transcript would offer a second reply box
	// against the same question.
	parent.ClarificationRequest = nil

	if err := s.messages.Update(ctx, parent); err != nil {
		return "", err
	}

	return pending + " " + strings.TrimSpace(content), nil
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
	loc *time.Location,
	emit Emitter,
) (*planner.Outcome, error) {
	service, ok := s.planner.(*planner.Service)
	if !ok {
		return s.planner.PlanIn(ctx, userID, content, loc)
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

	return service.PlanIn(ctx, userID, content, loc)
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
// complete finishes an assistant message with its content, reasoning and run.
//
// runID is uuid.Nil for a turn that produced no run — a question rather than a
// proposal — and is treated as absent rather than stored as a zero uuid, so a
// clarification is never mistaken for one awaiting approval.
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

	// A zero uuid is absence, not a link. Storing it would put a dangling run_id on a
	// message that produced no run.
	if runID != uuid.Nil {
		answer.WithRun(runID)
	}

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
