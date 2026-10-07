package domain

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Chat turns a thread of messages into replies.
//
// The agent's original shape was a run: one goal, one plan, one approval. That
// answers "do this thing" but not "actually, no, I meant this", because a run has
// nowhere to keep a revised request or the turns around it. A conversation does,
// and it is the shape a user already has a mental model for from every other tool
// they use.
const (
	// MessageRoleUser is something the user said.
	MessageRoleUser MessageRole = "user"

	// MessageRoleAssistant is something the assistant replied.
	MessageRoleAssistant MessageRole = "assistant"
)

// MessageRole is who produced a message.
type MessageRole string

// IsValid reports whether the role is one the domain defines.
//
// It is an enum rather than a free string because a role decides how a message is
// rendered and who may create it, so an unrecognised value is a bug rather than a
// new kind of turn.
func (r MessageRole) IsValid() bool {
	switch r {
	case MessageRoleUser, MessageRoleAssistant:
		return true
	default:
		return false
	}
}

// AllMessageRoles is the order used to build schemas and to validate.
var AllMessageRoles = []MessageRole{MessageRoleUser, MessageRoleAssistant}

// MaxMessageContentLength bounds one message.
//
// Bounded for the same reason a task title is: the model will happily produce a
// paragraph where a sentence was asked for, and an unbounded column lets a runaway
// generation become a storage problem the user did not cause and cannot see.
const MaxMessageContentLength = 8000

// Conversation is one thread of messages.
type Conversation struct {
	ID     uuid.UUID
	UserID uuid.UUID

	// Title is filled from the first user message and is nil until then. It is a
	// separate field rather than derived on read because the thread list needs a
	// label for conversations that have messages, and deriving it per row turns a
	// list into N queries.
	Title *string

	// HeadMessageID is the message at the end of the active branch, which is the
	// message the next turn answers.
	//
	// Stored rather than computed as the newest message because editing an earlier
	// message branches the thread: only the head of the chosen branch is the
	// conversation's current position.
	HeadMessageID *uuid.UUID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewConversation builds an untitled conversation owned by userID.
func NewConversation(userID uuid.UUID, now time.Time) (*Conversation, error) {
	if userID == uuid.Nil {
		return nil, Invalid("user id is required", map[string]string{"user_id": "must be set"})
	}

	return &Conversation{
		ID:        uuid.New(),
		UserID:    userID,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Touch records activity on the conversation.
func (c *Conversation) Touch(now time.Time) {
	c.UpdatedAt = now
}

// SetTitle labels the conversation, ignoring a blank one.
//
// A blank title is ignored rather than stored as empty, because the column is
// nullable to mean "not named yet" and an empty string would be a second way of
// saying the same thing, which is one more state to handle at every read.
func (c *Conversation) SetTitle(title string) {
	trimmed := strings.TrimSpace(title)

	if trimmed == "" {
		return
	}

	c.Title = &trimmed
}

// DerivedTitle is a short label for a conversation, falling back to a fixed string
// before it has any content.
func (c Conversation) DerivedTitle() string {
	if c.Title != nil && strings.TrimSpace(*c.Title) != "" {
		return *c.Title
	}

	return "New conversation"
}

// Message is one turn in a conversation.
//
// The table is a tree rather than a list. Revising a message inserts a new one
// beside it and marks the original superseded, so the wording the assistant was
// originally given survives to be read later. Overwriting would erase it, and that
// record is the whole point of letting a request be corrected.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	UserID         uuid.UUID

	// ParentID is the message this one answers or revises. It is nil only for the
	// opening message of a conversation.
	ParentID *uuid.UUID

	// SupersededMessageID is the message this one replaced, when this message is an
	// edit. It is set on the replacement, pointing back at the original.
	SupersededMessageID *uuid.UUID

	Role    MessageRole
	Content string

	// Reasoning is the model's own reasoning for this turn.
	//
	// It is nil whenever the model did not report any, and that is an ordinary
	// outcome rather than a gap to fill: a non-thinking model has none, and
	// synthesising a plausible-looking rationale afterwards would present an
	// explanation the model never gave as if it were the model's reasoning.
	Reasoning *string

	// RunID is set when this turn produced an agent run, which keeps the plan and
	// its approval reachable from the transcript.
	RunID *uuid.UUID

	// ClarificationRequest is set on an assistant message that asked a question
	// instead of answering, and holds the original request.
	//
	// The question is already Content. This holds the request it was asked about,
	// because the user's reply alone — "Tuesday at 4pm" — names no dentist, and
	// re-planning from it alone would lose the subject. It is nil on every other
	// message, and it is what makes a pending question resumable at all: the chat
	// service is stateless per request, so "is a question still outstanding?" has to
	// be answerable from the transcript rather than from memory.
	ClarificationRequest *string

	CreatedAt time.Time
}

// NewMessage builds a message on a conversation.
//
// It accepts the parent separately from the conversation because a message must
// belong to the same thread as its parent, and that is checked by the service: the
// constructor cannot see the parent's row.
func NewMessage(
	conversationID uuid.UUID,
	userID uuid.UUID,
	role MessageRole,
	content string,
	parent *uuid.UUID,
	now time.Time,
) (*Message, error) {
	if conversationID == uuid.Nil {
		return nil, Invalid("conversation id is required",
			map[string]string{"conversation_id": "must be set"})
	}

	if userID == uuid.Nil {
		return nil, Invalid("user id is required", map[string]string{"user_id": "must be set"})
	}

	if !role.IsValid() {
		return nil, Invalid("message role is not recognised",
			map[string]string{"role": "must be user or assistant"})
	}

	trimmed := strings.TrimSpace(content)

	if trimmed == "" {
		return nil, Invalid("message content is required",
			map[string]string{"content": "must not be empty"})
	}

	if len(trimmed) > MaxMessageContentLength {
		return nil, Invalid("message content is too long", map[string]string{
			"content": "must be at most " + strconv.Itoa(MaxMessageContentLength) + " characters",
		})
	}

	return &Message{
		ID:             uuid.New(),
		ConversationID: conversationID,
		UserID:         userID,
		ParentID:       parent,
		Role:           role,
		Content:        trimmed,
		CreatedAt:      now,
	}, nil
}

// NewPendingMessage creates an assistant message whose content is not written yet.
//
// It exists because the message id is needed before the reply is: a streaming
// client attaches each delta to a message as it arrives, so there has to be a
// message to attach to before generation starts. Creating the row up front also
// means an interrupted generation keeps whatever was produced instead of
// discarding it.
//
// It is deliberately separate from NewMessage rather than a special case of it,
// because "no content yet" is a real state that NewMessage must keep rejecting for
// ordinary callers: an empty user message is a bug, an empty assistant message in
// progress is not.
func NewPendingMessage(
	conversationID uuid.UUID,
	userID uuid.UUID,
	parent *uuid.UUID,
	now time.Time,
) (*Message, error) {
	if conversationID == uuid.Nil {
		return nil, Invalid("conversation id is required",
			map[string]string{"conversation_id": "must be set"})
	}

	if userID == uuid.Nil {
		return nil, Invalid("user id is required", map[string]string{"user_id": "must be set"})
	}

	return &Message{
		ID:             uuid.New(),
		ConversationID: conversationID,
		UserID:         userID,
		ParentID:       parent,
		Role:           MessageRoleAssistant,
		CreatedAt:      now,
	}, nil
}

// Complete writes the finished content.
//
// A message completed twice keeps the longer text rather than truncating to an
// earlier partial write: a retried completion arrives with the same prefix plus
// more, and the shorter of the two is by definition the incomplete one.
func (m *Message) Complete(content string) error {
	trimmed := strings.TrimSpace(content)

	if trimmed == "" {
		return Invalid("message content is required", map[string]string{"content": "must not be empty"})
	}

	if len(trimmed) > MaxMessageContentLength {
		return Invalid("message content is too long", map[string]string{
			"content": "must be at most " + strconv.Itoa(MaxMessageContentLength) + " characters",
		})
	}

	if len(trimmed) > len(m.Content) {
		m.Content = trimmed
	}

	return nil
}

// AsEdit returns a copy of m that replaces it: the same slot in the thread, marked
// as superseding the original.
//
// It is a copy rather than a mutation because the original has to stay readable.
// The new message takes the original's parent, so the branch moves one level up
// and everything the original was in conversation with becomes a sibling rather
// than an ancestor.
func (m Message) AsEdit(content string, now time.Time) (*Message, error) {
	edited, err := NewMessage(m.ConversationID, m.UserID, m.Role, content, m.ParentID, now)
	if err != nil {
		return nil, err
	}

	edited.SupersededMessageID = &m.ID

	return edited, nil
}

// WithReasoning records the model's reasoning for this turn.
//
// Empty reasoning is stored as nil so that "the model reported none" and "the
// model reported nothing" cannot be told apart at read time by guessing.
func (m *Message) WithReasoning(reasoning string) {
	trimmed := strings.TrimSpace(reasoning)

	if trimmed == "" {
		m.Reasoning = nil
		return
	}

	m.Reasoning = &trimmed
}

// WithRun links this turn to the run that carried out its intent.
func (m *Message) WithRun(runID uuid.UUID) {
	if runID == uuid.Nil {
		return
	}

	m.RunID = &runID
}

// DeriveTitle proposes a conversation title from a message.
//
// The first line is used rather than the whole message because a title is a label,
// and a label that runs to four hundred characters is not one. Character rather
// than rune counting matches the bounds used elsewhere in the domain.
func DeriveTitle(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}

	// A first line is usually a short summary in practice, but nothing stops a
	// client posting a single very long line, so it is still bounded.
	line := trimmed
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = line[:idx]
	}

	line = strings.TrimSpace(line)

	const limit = 60

	if len(line) <= limit {
		return line
	}

	cut := line[:limit]

	// Break on whitespace so the title does not end mid-word, which reads as
	// corruption rather than as truncation.
	if idx := strings.LastIndexAny(cut, " \t"); idx > limit/2 {
		return strings.TrimSpace(cut[:idx])
	}

	return strings.TrimSpace(cut)
}
