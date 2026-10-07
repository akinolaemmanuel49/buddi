package api

import (
	"strings"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// streamTurnRequest is one message sent to the chat surface.
type streamTurnRequest struct {
	// Message is what the user said.
	Message string `json:"message"`

	// ConversationID appends to an existing thread. Omitted starts a new one.
	ConversationID *uuid.UUID `json:"conversation_id,omitempty"`

	// ParentID is the message this turn answers or revises. Omitted uses the
	// conversation's current end.
	ParentID *uuid.UUID `json:"parent_id,omitempty"`

	// Mode forces how the turn is handled: "plan", "chat", or omitted to let the
	// server decide from the message.
	Mode string `json:"mode,omitempty"`

	// Edit makes this turn revise ParentID instead of appending.
	Edit bool `json:"edit,omitempty"`
}

// messageResponse is one turn as the client renders it.
type messageResponse struct {
	ID             string  `json:"id"`
	ConversationID string  `json:"conversation_id"`
	ParentID       *string `json:"parent_id,omitempty"`
	Role           string  `json:"role"`
	Content        string  `json:"content"`
	Reasoning      *string `json:"reasoning,omitempty"`
	RunID          *string `json:"run_id,omitempty"`
	CreatedAt      string  `json:"created_at"`

	// AwaitingAnswer marks a message that asked a question rather than answering, so a
	// reloaded transcript can still offer the reply box that a live stream does.
	//
	// Without it, reloading a thread would turn an outstanding question into an ordinary
	// line of text and the user would have no way to tell that something is waiting on
	// them.
	AwaitingAnswer bool `json:"awaiting_answer,omitempty"`
}

// conversationResponse is one thread in the list.
type conversationResponse struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	HeadMessageID *string `json:"head_message_id,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type conversationListResponse struct {
	Data   []conversationResponse `json:"data"`
	Total  int64                  `json:"total"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

type conversationDetailResponse struct {
	Conversation conversationResponse `json:"conversation"`
	Messages     []messageResponse    `json:"messages"`
}

// eventPayload is one server-sent event.
//
// The shape is flat with omitempty throughout, because these are written once per
// token: a fixed shape full of nulls would be several times the bytes for every
// fragment of every reply.
type eventPayload struct {
	ConversationID string `json:"conversation_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	QuestionID     string `json:"question_id,omitempty"`

	// Mode is how the server resolved this turn, sent on the start event so a client
	// can tell a streamed plan from a streamed answer.
	Mode string `json:"mode,omitempty"`

	Delta     string `json:"delta,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`

	// ReasoningAvailable reports that this model exposes reasoning. A client needs
	// it to distinguish "not started yet" from "this model never will", which look
	// identical in an empty panel.
	ReasoningAvailable bool `json:"reasoning_available,omitempty"`

	// ReasoningComplete is the reasoning as a whole, at the end of the turn.
	ReasoningComplete string `json:"reasoning_complete,omitempty"`

	Content  string `json:"content,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`

	// Clarification is set on a turn that asked a question instead of proposing
	// anything. Absent on every other turn.
	Clarification *clarificationPayload `json:"clarification,omitempty"`

	Run *runRefPayload `json:"run,omitempty"`
}

// clarificationPayload is a question put to the user.
//
// It carries the request as well as the question, because the two answer different
// needs: the question is what the user reads, and the request is what says which of
// several things in the conversation the question is about.
type clarificationPayload struct {
	Question string `json:"question"`
	Request  string `json:"request,omitempty"`
}

type runRefPayload struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func toEventPayload(event chat.Event) eventPayload {
	payload := eventPayload{
		Delta:              event.Delta,
		Reasoning:          event.Reasoning,
		ReasoningAvailable: event.ReasoningAvailable,
		ReasoningComplete:  event.ReasoningComplete,
		Content:            event.Content,
		Fallback:           event.Fallback,
	}

	if event.ConversationID != uuid.Nil {
		payload.ConversationID = event.ConversationID.String()
	}

	if event.MessageID != uuid.Nil {
		payload.MessageID = event.MessageID.String()
	}

	if event.QuestionID != uuid.Nil {
		payload.QuestionID = event.QuestionID.String()
	}

	if event.Mode != "" {
		payload.Mode = string(event.Mode)
	}

	if event.Clarification != nil {
		payload.Clarification = &clarificationPayload{
			Question: event.Clarification.Question,
			Request:  event.Clarification.Request,
		}
	}

	if event.Run != nil {
		payload.Run = &runRefPayload{ID: event.Run.ID.String(), Status: event.Run.Status}
	}

	return payload
}

func toConversationResponse(conversation *domain.Conversation) conversationResponse {
	response := conversationResponse{
		ID:        conversation.ID.String(),
		Title:     conversation.DerivedTitle(),
		CreatedAt: conversation.CreatedAt.Format(timeLayout),
		UpdatedAt: conversation.UpdatedAt.Format(timeLayout),
	}

	if conversation.HeadMessageID != nil {
		head := conversation.HeadMessageID.String()
		response.HeadMessageID = &head
	}

	return response
}

func toMessageResponses(messages []domain.Message) []messageResponse {
	responses := make([]messageResponse, 0, len(messages))

	for _, message := range messages {
		responses = append(responses, toMessageResponse(message))
	}

	return responses
}

func toMessageResponse(message domain.Message) messageResponse {
	response := messageResponse{
		ID:             message.ID.String(),
		ConversationID: message.ConversationID.String(),
		Role:           string(message.Role),
		Content:        message.Content,
		Reasoning:      message.Reasoning,
		CreatedAt:      message.CreatedAt.Format(timeLayout),

		// Only an assistant message can be a question, and only a stored request
		// makes one resumable. Both are checked because a cleared marker on an
		// assistant message means the user already answered it.
		AwaitingAnswer: message.Role == domain.MessageRoleAssistant &&
			message.ClarificationRequest != nil &&
			strings.TrimSpace(*message.ClarificationRequest) != "",
	}

	if message.ParentID != nil {
		parent := message.ParentID.String()
		response.ParentID = &parent
	}

	if message.RunID != nil {
		run := message.RunID.String()
		response.RunID = &run
	}

	return response
}
