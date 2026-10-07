package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tools is the connector's tool surface: one read, one write.
type Tools struct {
	api   API
	token TokenSource
}

// NewTools builds the connector's tools.
func NewTools(api API, token TokenSource) *Tools {
	return &Tools{api: api, token: token}
}

// ToolHandler pairs a tool's definition with its handler.
type ToolHandler struct {
	Tool Tool

	// HandleFunc receives the arguments as approved. It is given the user so it can
	// resolve that user's own credential; a connector must never use a shared token.
	HandleFunc func(ctx context.Context, userID string, arguments json.RawMessage) (any, error)
}

// Tool is a tool as the connector describes it.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage

	// ReadOnly marks a tool that changes nothing. It is what stops a plan from
	// resolving to it: there is no change for a user to approve.
	ReadOnly bool
}

// Handlers returns every tool with its handler.
//
// The definitions live beside the handlers rather than being written out separately
// because a tool the registry cannot describe is a tool nobody can discover, and a
// description that drifts from behaviour is how a model ends up calling the wrong
// thing.
func (t *Tools) Handlers() []ToolHandler {
	return []ToolHandler{
		{
			Tool: Tool{
				Name:        ListEventsTool,
				Description: "List calendar events in a time window. Read-only: it changes nothing.",
				InputSchema: listEventsSchema,
				ReadOnly:    true,
			},
			HandleFunc: t.listEvents,
		},
		{
			Tool: Tool{
				Name:        CreateEventTool,
				Description: "Create a calendar event.",
				InputSchema: createEventSchema,
				ReadOnly:    false,
			},
			HandleFunc: t.createEvent,
		},
	}
}

// listEvents answers a bounded read.
func (t *Tools) listEvents(ctx context.Context, userID string, arguments json.RawMessage) (any, error) {
	query, err := QueryFromArguments(arguments)
	if err != nil {
		return nil, err
	}

	accessToken, err := t.token.AccessToken(ctx, userID)
	if err != nil {
		return nil, err
	}

	events, err := t.api.ListEvents(ctx, accessToken, query)
	if err != nil {
		return nil, err
	}

	if events == nil {
		// An empty result is an answer, not an absence of one. A nil slice would
		// encode as null and read as "the tool failed to produce a list".
		events = []Event{}
	}

	return map[string]any{"events": events, "count": len(events)}, nil
}

// createEvent inserts one event.
//
// The arguments are decoded into an Event and validated field by field rather than
// forwarded as raw bytes. That is what makes "what the user approved is what Google
// received" a property of the code: the object the provider gets is one this package
// understood completely, and an argument it did not understand was refused rather
// than dropped.
func (t *Tools) createEvent(ctx context.Context, userID string, arguments json.RawMessage) (any, error) {
	event, err := EventFromArguments(arguments)
	if err != nil {
		return nil, err
	}

	if err := event.Validate(); err != nil {
		return nil, err
	}

	accessToken, err := t.token.AccessToken(ctx, userID)
	if err != nil {
		return nil, err
	}

	created, err := t.api.CreateEvent(ctx, accessToken, event)
	if err != nil {
		return nil, err
	}

	if created == nil {
		// The provider accepted the call and returned nothing. Reporting success
		// without an event id would leave a run looking complete when the user cannot
		// tell whether anything was created.
		return nil, Refused("the calendar provider returned no event")
	}

	result := map[string]any{
		"summary":  created.Summary,
		"start_at": created.StartAt,
	}

	if id := created.ID(); id != "" {
		result["event_id"] = id
	}

	if link := created.Link(); link != "" {
		result["html_link"] = link
	}

	return result, nil
}

// EncodeEvent renders an Event as tool arguments.
//
// Used by the encoder that turns a plan into a calendar proposal, and exported so the
// approved payload has one definition rather than a struct literal repeated on both
// sides of the approval boundary.
func EncodeEvent(event Event) (json.RawMessage, error) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, Refused("could not encode event arguments: %v", err)
	}

	return encoded, nil
}

// CreatedEvent builds an event with a provider identity attached, which is what a
// fake provider returns.
func CreatedEvent(summary string, start time.Time, id string) *Event {
	event := &Event{
		Summary: summary,
		StartAt: start.UTC().Format(time.RFC3339),
	}

	event.SetIdentity(id, fmt.Sprintf("https://calendar.google.com/event?eid=%s", id))

	return event
}

// Trimmed returns the event with whitespace removed from its text fields, which is
// what gets stored: a summary of "  Deploy freeze  " should not render with the
// padding in the user's calendar.
func (e Event) Trimmed() Event {
	e.Summary = strings.TrimSpace(e.Summary)
	e.Description = strings.TrimSpace(e.Description)
	e.Location = strings.TrimSpace(e.Location)
	e.CalendarID = strings.TrimSpace(e.CalendarID)
	e.StartAt = strings.TrimSpace(e.StartAt)
	e.EndAt = strings.TrimSpace(e.EndAt)
	e.TimeZone = strings.TrimSpace(e.TimeZone)

	return e
}
