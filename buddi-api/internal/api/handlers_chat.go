package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
)

// sseErrorGrace is how long the stream stays open after a failure.
//
// Not zero: the error event is written immediately, but a response closed in the
// same tick can take the buffered event with it, leaving the client with a stream
// that ended for no stated reason.
const sseErrorGrace = 50 * time.Millisecond

// handleStreamTurn appends a message to a conversation and streams the reply.
//
// This is the whole reason the chat surface exists: the reply arrives as it is
// generated, so a model that needs ten seconds says something in the first one.
func (a *API) handleStreamTurn(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	var body streamTurnRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	turn := chat.Turn{
		Input:          body.Message,
		Mode:           chat.Mode(body.Mode),
		ConversationID: body.ConversationID,
		ParentID:       body.ParentID,
		Edit:           body.Edit,
	}

	// Validated before the stream opens, because opening it commits the response to
	// 200. A malformed request answered with a stream and an error event would be a
	// status code a client cannot act on.
	if err := chat.ValidateTurn(turn); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	stream, err := a.newTurnStream(w, r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	_, turnErr := a.chat.Turn(r.Context(), userID, turn, stream.emit)

	stream.close(r, turnErr)
}

// handleListConversations returns the user's threads, most recently touched first.
func (a *API) handleListConversations(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	page, err := Pagination(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	conversations, total, err := a.chat.ListConversations(r.Context(), userID, page.Limit, page.Offset)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	data := make([]conversationResponse, 0, len(conversations))
	for i := range conversations {
		data = append(data, toConversationResponse(&conversations[i]))
	}

	WriteJSON(w, r, http.StatusOK, conversationListResponse{
		Data:   data,
		Total:  total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

// handleGetConversation returns one thread with its current messages.
func (a *API) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	id, err := uuid.Parse(PathParam(r, "id"))
	if err != nil {
		WriteError(w, r, NotFound("no conversation with that id"))

		return
	}

	conversation, messages, err := a.chat.GetConversation(r.Context(), userID, id)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, conversationDetailResponse{
		Conversation: toConversationResponse(conversation),
		Messages:     toMessageResponses(messages),
	})
}

// handleDeleteConversation removes a thread and everything in it.
func (a *API) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	id, err := uuid.Parse(PathParam(r, "id"))
	if err != nil {
		WriteError(w, r, NotFound("no conversation with that id"))

		return
	}

	if err := a.chat.DeleteConversation(r.Context(), userID, id); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// turnStream writes chat events as server-sent events.
type turnStream struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	controller *http.ResponseController
}

// newTurnStream prepares the response for streaming.
//
// The write deadline is cleared before the first byte. The server's WriteTimeout is
// sized for a buffered response and would cut this connection off part way through a
// reply, which on a CPU runtime is most of it. The stream is bounded by the request
// context instead, which a client cancelling ends immediately.
func (a *API) newTurnStream(w http.ResponseWriter, r *http.Request) (*turnStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, events queue until the turn ends, which is exactly what
		// this endpoint exists not to do. Refusing beats serving a stream that only
		// arrives all at once.
		return nil, Internal("this connection cannot stream a response")
	}

	controller := http.NewResponseController(w)

	// A zero time means no deadline, releasing the server-wide write timeout for this
	// response only. A failure is not fatal: the stream still works, bounded by
	// whatever remains of the original timeout.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		a.log.Warn("could not clear the write deadline for a stream",
			"error", err, "path", r.URL.Path)
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	// Reverse proxies buffer streamed responses unless told not to, which would put
	// the wait straight back.
	header.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &turnStream{w: w, flusher: flusher, controller: controller}, nil
}

// emit forwards a chat event.
func (s *turnStream) emit(event chat.Event) error {
	payload, err := json.Marshal(toEventPayload(event))
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
		return err
	}

	s.flusher.Flush()

	return nil
}

// close reports a failed turn and ends the stream.
//
// A turn the client cancelled is not reported: the client that cancelled it is the
// only one that would receive the event, and it already knows.
func (s *turnStream) close(r *http.Request, turnErr error) {
	if turnErr == nil {
		return
	}

	if errors.Is(turnErr, context.Canceled) || errors.Is(turnErr, context.DeadlineExceeded) &&
		r.Context().Err() != nil {
		return
	}

	apiErr := serviceError(r, turnErr)

	body, err := json.Marshal(errorResponse{Error: errorBody{
		Code:    apiErr.Code,
		Message: apiErr.Message,
	}})
	if err != nil {
		return
	}

	_, _ = fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", body)
	s.flusher.Flush()

	time.Sleep(sseErrorGrace)
}
