package calendar

import "encoding/json"

// The tools' JSON Schemas, written out rather than derived from the Go structs.
//
// Deriving them would have made the schema and the decoder drift together: a field
// added to Event for the provider's benefit would silently become acceptable input,
// and §10.2 depends on the approved payload having exactly the fields the user was
// shown. Writing them by hand means the surface the model sees is a decision, and a
// new field has to be added here on purpose.
var (
	createEventSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {
      "type": "string",
      "description": "Short title for the event. Required.",
      "maxLength": 500
    },
    "start_at": {
      "type": "string",
      "description": "RFC 3339 start timestamp, for example 2026-03-04T14:00:00Z. Required.",
      "format": "date-time"
    },
    "end_at": {
      "type": "string",
      "description": "RFC 3339 end timestamp. Optional; a point-in-time event may omit it.",
      "format": "date-time"
    },
    "timezone": {
      "type": "string",
      "description": "IANA time zone the times are stated in, for example Europe/London."
    },
    "description": {
      "type": "string",
      "description": "Brief detail about the event. Optional.",
      "maxLength": 8192
    },
    "location": {
      "type": "string",
      "description": "Where the event happens. Optional.",
      "maxLength": 1024
    },
    "attendees": {
      "type": "array",
      "description": "Email addresses to invite. Optional.",
      "items": { "type": "string", "maxLength": 254 },
      "maxItems": 50
    },
    "calendar_id": {
      "type": "string",
      "description": "Calendar to write to. Optional; defaults to the primary calendar."
    }
  },
  "required": ["summary", "start_at"],
  "additionalProperties": false
}`)

	listEventsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "time_min": {
      "type": "string",
      "description": "RFC 3339 start of the window to read. Required.",
      "format": "date-time"
    },
    "time_max": {
      "type": "string",
      "description": "RFC 3339 end of the window to read. Required.",
      "format": "date-time"
    },
    "calendar_id": {
      "type": "string",
      "description": "Calendar to read. Optional; defaults to the primary calendar."
    },
    "query": {
      "type": "string",
      "description": "Free-text term to match against event titles. Optional."
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum events to return, capped at 250. Optional.",
      "minimum": 0,
      "maximum": 250
    },
    "single_events": {
      "type": "boolean",
      "description": "Expand recurring events into individual occurrences. Optional."
    }
  },
  "required": ["time_min", "time_max"],
  "additionalProperties": false
}`)
)

// CreateEventSchema and ListEventsSchema expose the schemas so a caller that
// documents the tools elsewhere describes the same tools.
func CreateEventSchema() json.RawMessage { return createEventSchema }

// ListEventsSchema exposes the read tool's schema.
func ListEventsSchema() json.RawMessage { return listEventsSchema }
