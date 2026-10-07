// Package calendar is the Google Calendar connector's tool surface.
//
// It defines what a calendar call looks like — the tools, their arguments, and the
// provider behind them — and nothing else. The transport is supplied by the caller,
// so the same tools are reached over stdio as a separate process and in-process
// during tests.
//
// The one rule everything here serves: what reaches Google is exactly the payload
// the user approved. The arguments are decoded strictly, so a payload carrying note
// content cannot pass through as an unrecognised key, and the provider is handed the
// decoded struct rather than the raw bytes, which is what makes "the reviewed bytes
// are the sent bytes" a property of the code rather than a habit.
package calendar

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tool names. They are stable identifiers on the approval row, so they must not
// change once a proposal has been shown to a user.
const (
	// ListEventsTool reads events. It changes nothing, so it is read-only and a plan
	// can never resolve to it.
	ListEventsTool = "calendar.list_events"

	// CreateEventTool writes an event.
	CreateEventTool = "calendar.create_event"
)

// Refusal separates a tool declining from a call failing.
//
// A connector saying "not authorised" or "that time is invalid" is an ordinary
// answer that the agent should surface to the user. Reporting it as a transport
// failure would read as the connector being broken, which is a different problem
// with a different fix.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// Refused builds a Refusal.
func Refused(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a tool declining.
func IsRefusal(err error) bool {
	var refusal *Refusal

	return errors.As(err, &refusal)
}

// Event is what calendar.create_event sends and what calendar.list_events returns.
//
// It is the whole vocabulary of a calendar call in this system. There is no field
// for free text of any length and no place for a note's content, which is how §10.2
// is enforced rather than merely intended.
type Event struct {
	// Summary is the event title. Bounded because Google rejects an empty one and an
	// unbounded one would be whatever the model produced.
	Summary string `json:"summary"`

	// Description is optional detail. It is the planner's own words about the event,
	// never retrieved note content.
	Description string `json:"description,omitempty"`

	// StartAt and EndAt are RFC 3339 timestamps. An event with no end is treated by
	// Google as a point in time, so EndAt is optional but must not precede StartAt.
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at,omitempty"`

	// TimeZone is an IANA zone name. Recorded rather than assumed: a local server's
	// zone is the container's, which is almost never the user's.
	TimeZone string `json:"timezone,omitempty"`

	// Location is free text the user typed or the planner summarised.
	Location string `json:"location,omitempty"`

	// CalendarID selects a calendar other than the primary one.
	CalendarID string `json:"calendar_id,omitempty"`

	// Attendees are email addresses. Validated here rather than by the provider so a
	// typo is reported before the user approves it.
	Attendees []string `json:"attendees,omitempty"`

	// id and link are the provider's identifiers for an event it created or read.
	//
	// Unexported and outside the JSON shape on purpose: an id is assigned by Google,
	// so accepting one as input would let an approved payload name the event it
	// writes. Being outside the strict decoder means a payload carrying one is
	// refused, not silently discarded.
	id   string
	link string
}

// SetIdentity records the provider's identifiers for an event.
func (e *Event) SetIdentity(id string, link string) {
	e.id = strings.TrimSpace(id)
	e.link = strings.TrimSpace(link)
}

// ID returns the provider's event id, empty for an event this process never fetched.
func (e *Event) ID() string { return e.id }

// Link returns the provider's URL for the event.
func (e *Event) Link() string { return e.link }

// Field limits. Google rejects an empty summary and one over 500 characters, and an
// event title is a title: these are the provider's own limits, so a proposal outside
// them would be shown to the user and then refused at execution.
const (
	MaxSummaryLength     = 500
	MaxDescriptionLength = 8192
	MaxLocationLength    = 1024
	MaxAttendees         = 50
)

// MaxAttendeeLength is Google's own address limit, which is generous for what a
// calendar invite needs.
const MaxAttendeeLength = 254

// Validate checks an event against the provider's rules.
//
// This runs before the proposal is shown, so a user is never asked to approve a
// payload that would then be refused.
func (e *Event) Validate() error {
	summary := strings.TrimSpace(e.Summary)

	switch {
	case summary == "":
		return Refused("summary is required")
	case len([]rune(summary)) > MaxSummaryLength:
		return Refused("summary must be at most %d characters", MaxSummaryLength)
	}

	if len([]rune(e.Description)) > MaxDescriptionLength {
		return Refused("description must be at most %d characters", MaxDescriptionLength)
	}

	if len([]rune(e.Location)) > MaxLocationLength {
		return Refused("location must be at most %d characters", MaxLocationLength)
	}

	start, err := e.parseStart()
	if err != nil {
		return err
	}

	// An absent end is a point-in-time event, which Google allows. Only compare when
	// one was given: the zero time is before every real start, so comparing
	// unconditionally would reject every event that legitimately has no end.
	if strings.TrimSpace(e.EndAt) != "" {
		end, err := e.parseEnd()
		if err != nil {
			return err
		}

		if end.Before(start) {
			return Refused("end_at must not be before start_at")
		}
	}

	if len(e.Attendees) > MaxAttendees {
		return Refused("at most %d attendees are allowed", MaxAttendees)
	}

	for _, attendee := range e.Attendees {
		trimmed := strings.TrimSpace(attendee)

		if trimmed == "" {
			return Refused("attendee addresses must not be empty")
		}

		if len(trimmed) > MaxAttendeeLength {
			return Refused("attendee address must be at most %d characters", MaxAttendeeLength)
		}

		if !strings.Contains(trimmed, "@") {
			return Refused("attendee %q is not an email address", attendee)
		}
	}

	return nil
}

func (e *Event) parseStart() (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(e.StartAt))
	if err != nil {
		return time.Time{}, Refused("start_at must be an RFC 3339 timestamp")
	}

	return parsed, nil
}

func (e *Event) parseEnd() (time.Time, error) {
	trimmed := strings.TrimSpace(e.EndAt)
	if trimmed == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, Refused("end_at must be an RFC 3339 timestamp")
	}

	return parsed, nil
}

// MaxListResults caps a listing.
//
// Google's own page limit is 2500. A lower cap is set because a plan proposes one
// read, and 2500 events is roughly a note's worth of text on a single line of the
// protocol: the bound is about keeping an answer legible, not about being polite to
// the provider.
const MaxListResults = 250

func parseRequired(field, value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)

	if trimmed == "" {
		return time.Time{}, Refused("%s is required", field)
	}

	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, Refused("%s must be an RFC 3339 timestamp", field)
	}

	return parsed, nil
}

// decodeStrict decodes arguments with unknown fields refused.
//
// This is the enforcement point for §10.2. Without it a payload could carry note
// content in a key the tool ignores, pass validation, be shown to the user as
// something else entirely, and then be dropped — leaving the appearance that
// approved bytes reached Google when what reached it was nothing at all. Refusing
// the payload makes the mismatch impossible rather than merely unlikely.
//
// The keys are checked by name before decoding, and the decoder is not relied on to do
// it. encoding/json matches field names case-insensitively and prefers an exact match,
// so "Start_At" would be read as start_at: a payload could then carry both, the
// approved one would be silently overridden, and the value the user reviewed would not
// be the value sent. An exact key match makes the payload the user saw the only one
// this code can read.
func decodeStrict(raw json.RawMessage, allowed []string, target any) error {
	permitted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		permitted[name] = struct{}{}
	}

	var fields map[string]json.RawMessage

	if err := json.Unmarshal(raw, &fields); err != nil {
		return Refused("arguments must be a single JSON object: %v", err)
	}

	for name := range fields {
		if _, ok := permitted[name]; !ok {
			return Refused("argument %q is not part of this tool", name)
		}
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return Refused("arguments are not valid tool input: %v", err)
	}

	// A second value means the payload was not one JSON object, which is a payload
	// nobody reviewed.
	if decoder.More() {
		return Refused("arguments must be a single JSON object")
	}

	return nil
}
