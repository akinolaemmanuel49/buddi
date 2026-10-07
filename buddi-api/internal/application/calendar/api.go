package calendar

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// API is the Google Calendar surface these tools need.
//
// It is an interface with two methods because that is the actual dependency, and
// because it is what makes the connector testable without a Google account: the
// tools' behaviour under an expired token, a revoked grant or an odd provider
// response is the interesting part, and none of it needs a network.
type API interface {
	// CreateEvent inserts an event and returns it as the provider stored it.
	CreateEvent(ctx context.Context, accessToken string, event Event) (*Event, error)

	// ListEvents returns the events in a window.
	ListEvents(ctx context.Context, accessToken string, query ListQuery) ([]Event, error)
}

// ListQuery is a bounded read.
type ListQuery struct {
	CalendarID   string
	TimeMin      time.Time
	TimeMax      time.Time
	MaxResults   int
	SearchTerm   string
	SingleEvents bool
}

// TokenSource supplies a live access token for one user.
//
// It is the seam between this package and the OAuth store. The connector never reads
// a token from a database and never holds one beyond the call that uses it.
type TokenSource interface {
	AccessToken(ctx context.Context, userID string) (string, error)
}

// listEventsPayload is the read tool's approved arguments.
//
// It is a separate type from ListQuery so the wire shape and the provider call cannot
// be the same thing: a change to Google's API then has to be translated deliberately
// rather than arriving as a change to what the user approved.
type listEventsPayload struct {
	CalendarID string `json:"calendar_id,omitempty"`

	// TimeMin and TimeMax bound the window. Required: an unbounded listing over a real
	// calendar has no useful page size and would make a read tool a way to pull an
	// entire account's history.
	TimeMin      string `json:"time_min"`
	TimeMax      string `json:"time_max"`
	MaxResults   int    `json:"max_results,omitempty"`
	Query        string `json:"query,omitempty"`
	SingleEvents bool   `json:"single_events,omitempty"`
}

// validate bounds a listing before it is shown to a user, so a proposal cannot be
// approved and then refused for a limit that was knowable up front.
func (p *listEventsPayload) validate() error {
	if strings.TrimSpace(p.CalendarID) != "" && len(p.CalendarID) > 512 {
		return Refused("calendar_id must be at most 512 characters")
	}

	if _, err := parseRequired("time_min", p.TimeMin); err != nil {
		return err
	}

	max, err := parseRequired("time_max", p.TimeMax)
	if err != nil {
		return err
	}

	min, _ := parseRequired("time_min", p.TimeMin)

	if !max.After(min) {
		return Refused("time_max must be after time_min")
	}

	if p.MaxResults < 0 {
		return Refused("max_results must not be negative")
	}

	switch {
	case p.MaxResults == 0:
		// A default rather than a refusal: "as many as you need" is the natural way
		// to ask for a window, and 50 covers a working week.
		p.MaxResults = DefaultListResults
	case p.MaxResults > MaxListResults:
		p.MaxResults = MaxListResults
	}

	return nil
}

// DefaultListResults is the page size when a caller does not ask for one.
const DefaultListResults = 50

// listEventsArguments are the exact keys calendar.list_events accepts.
var listEventsArguments = []string{
	"calendar_id",
	"time_min",
	"time_max",
	"max_results",
	"query",
	"single_events",
}

// AccessToken resolves a user's token for the OAuth store. It satisfies TokenSource.
type AccessToken func(ctx context.Context, userID string) (string, error)

// EventArguments renders an Event as the tool's arguments.
//
// Used by the encoder that turns a plan into a calendar proposal, and exported so
// the encoding is one definition rather than a struct literal repeated on both sides
// of the approval boundary.
func EventArguments(event Event) (json.RawMessage, error) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, Refused("could not encode event arguments: %v", err)
	}

	return encoded, nil
}

// eventArguments are the exact keys calendar.create_event accepts.
//
// Named here rather than derived from Event's tags so the accepted set is a decision
// someone made, and so adding a field to Event for the provider's benefit does not
// quietly widen what a user can approve.
var eventArguments = []string{
	"summary",
	"start_at",
	"end_at",
	"timezone",
	"description",
	"location",
	"attendees",
	"calendar_id",
}

// EventFromArguments decodes a payload strictly. It is the inverse of EncodeEvent.
func EventFromArguments(raw json.RawMessage) (Event, error) {
	var event Event

	if err := decodeStrict(raw, eventArguments, &event); err != nil {
		return Event{}, err
	}

	return event, nil
}

// QueryFromArguments decodes a listing payload strictly.
func QueryFromArguments(raw json.RawMessage) (ListQuery, error) {
	var payload listEventsPayload

	if err := decodeStrict(raw, listEventsArguments, &payload); err != nil {
		return ListQuery{}, err
	}

	if err := payload.validate(); err != nil {
		return ListQuery{}, err
	}

	min, err := parseRequired("time_min", payload.TimeMin)
	if err != nil {
		return ListQuery{}, err
	}

	max, err := parseRequired("time_max", payload.TimeMax)
	if err != nil {
		return ListQuery{}, err
	}

	return ListQuery{
		CalendarID:   strings.TrimSpace(payload.CalendarID),
		TimeMin:      min,
		TimeMax:      max,
		MaxResults:   payload.MaxResults,
		SearchTerm:   strings.TrimSpace(payload.Query),
		SingleEvents: payload.SingleEvents,
	}, nil
}
