package calendar_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
)

// fakeAPI stands in for Google. It records exactly what it was handed, because the
// question these tests exist to answer is not "did the tool succeed" but "what would
// Google have received".
type fakeAPI struct {
	created []calendar.Event
	token   string
	query   calendar.ListQuery

	createdResult *calendar.Event
	listResult    []calendar.Event

	createErr error
	listErr   error

	// createNothing makes CreateEvent accept the call and answer with no event and no
	// error, which is what a provider returning an empty body looks like from here.
	createNothing bool
}

func (f *fakeAPI) CreateEvent(_ context.Context, accessToken string, event calendar.Event) (*calendar.Event, error) {
	f.created = append(f.created, event)
	f.token = accessToken

	if f.createErr != nil {
		return nil, f.createErr
	}

	if f.createNothing {
		return nil, nil
	}

	if f.createdResult != nil {
		return f.createdResult, nil
	}

	// The tool has already validated start_at by the time this runs, so a parse
	// failure here would mean the validation was skipped rather than that the fixture
	// is wrong. Panicking makes that visible instead of letting a zero time through.
	start, err := time.Parse(time.RFC3339, event.StartAt)
	if err != nil {
		panic("CreateEvent received an unvalidated start_at: " + err.Error())
	}

	return calendar.CreatedEvent(event.Summary, start, "evt-1"), nil
}

func (f *fakeAPI) ListEvents(_ context.Context, accessToken string, query calendar.ListQuery) ([]calendar.Event, error) {
	f.query = query
	f.token = accessToken

	return f.listResult, f.listErr
}

// fakeTokens hands out one token per user so a test can prove a session uses its own
// user's credential.
type fakeTokens struct {
	byUser map[string]string
	err    error
}

func (f *fakeTokens) AccessToken(_ context.Context, userID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}

	token, ok := f.byUser[userID]
	if !ok {
		return "", calendar.Refused("calendar is not connected for this account")
	}

	return token, nil
}

// newTools builds the connector over the fakes above.
func newTools(t *testing.T, api calendar.API, tokens calendar.TokenSource) *calendar.Tools {
	t.Helper()

	return calendar.NewTools(api, tokens)
}

const user = "user-1"

// approvedEvent is a payload shaped like one the planner produces.
const approvedEvent = `{"summary":"Deploy freeze","start_at":"2026-03-04T14:00:00Z","end_at":"2026-03-04T15:00:00Z","timezone":"Europe/London"}`

// The approved bytes have to be the bytes the provider is handed. This is the whole
// point of the connector, so it is asserted against the decoded struct field by field
// rather than by comparing a JSON string, which would pass even if the two disagreed
// on formatting.
func TestCreateEventSendsTheApprovedPayload(t *testing.T) {
	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	result, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(approvedEvent))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(api.created) != 1 {
		t.Fatalf("CreateEvent calls = %d, want 1", len(api.created))
	}

	sent := api.created[0]

	want := calendar.Event{
		Summary:  "Deploy freeze",
		StartAt:  "2026-03-04T14:00:00Z",
		EndAt:    "2026-03-04T15:00:00Z",
		TimeZone: "Europe/London",
	}

	if sent.Summary != want.Summary || sent.StartAt != want.StartAt ||
		sent.EndAt != want.EndAt || sent.TimeZone != want.TimeZone {
		t.Errorf("provider received %+v, want %+v", sent, want)
	}

	if sent.Description != "" {
		t.Errorf("description = %q, want empty: nothing beyond the payload was added", sent.Description)
	}

	if api.token != "token-abc" {
		t.Errorf("token = %q, want the user's own", api.token)
	}

	if id, ok := result.(map[string]any)["event_id"].(string); !ok || id != "evt-1" {
		t.Errorf("result event_id = %v, want the provider's id", result)
	}
}

// A payload carrying a field the tool does not define must be refused. If it were
// accepted and ignored, a proposal could show one set of arguments and the provider
// would receive another, which is the exact failure §10.2 exists to prevent.
func TestCreateEventRefusesAPayloadCarryingUnknownFields(t *testing.T) {
	cases := map[string]string{
		"note content":     `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","notes":"secret journal entry"}`,
		"event id":         `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","event_id":"evt-someone-elses"}`,
		"nested content":   `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","attachment":{"body":"a note body"}}`,
		"shadowed typo":    `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","Start_At":"2026-01-01T00:00:00Z"}`,
		"second document":  `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z"}{"summary":"other"}`,
		"not an object":    `["summary","start_at"]`,
		"wrong json types": `{"summary":42,"start_at":"2026-03-04T09:00:00Z"}`,
	}

	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(payload))
			if err == nil {
				t.Fatalf("Handle(%s) succeeded, want a refusal", payload)
			}

			if !calendar.IsRefusal(err) {
				t.Errorf("error %v is not a refusal, so the model is told the connector is broken", err)
			}
		})
	}

	if len(api.created) != 0 {
		t.Errorf("provider was called %d times, want 0: a refused payload must not reach Google", len(api.created))
	}
}

// Validation runs before the user is asked to approve anything.
func TestCreateEventRefusesPayloadsGoogleWouldReject(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    string
	}{
		"empty summary": {
			payload: `{"summary":"   ","start_at":"2026-03-04T09:00:00Z"}`,
			want:    "summary is required",
		},
		"summary too long": {
			payload: `{"summary":"` + strings.Repeat("x", MaxSummary+1) + `","start_at":"2026-03-04T09:00:00Z"}`,
			want:    "summary must be at most",
		},
		"missing start": {
			payload: `{"summary":"Standup"}`,
			want:    "start_at must be an RFC 3339 timestamp",
		},
		"unparseable start": {
			payload: `{"summary":"Standup","start_at":"next tuesday"}`,
			want:    "start_at must be an RFC 3339 timestamp",
		},
		"end before start": {
			payload: `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","end_at":"2026-03-04T08:00:00Z"}`,
			want:    "end_at must not be before start_at",
		},
		"attendee is not an address": {
			payload: `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","attendees":["sam"]}`,
			want:    "is not an email address",
		},
		"empty attendee": {
			payload: `{"summary":"Standup","start_at":"2026-03-04T09:00:00Z","attendees":["  "]}`,
			want:    "must not be empty",
		},
	}

	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(test.payload))
			if err == nil {
				t.Fatal("Handle succeeded, want a refusal")
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to mention %q", err, test.want)
			}
		})
	}

	if len(api.created) != 0 {
		t.Errorf("provider was called %d times, want 0", len(api.created))
	}
}

// Limits mirror the constraints in the package, so the test states them against the
// boundary rather than a copied number that can drift from the code.
const (
	MaxSummary     = calendar.MaxSummaryLength
	MaxDescription = calendar.MaxDescriptionLength
	MaxLocation    = calendar.MaxLocationLength
)

func TestCreateEventAcceptsPayloadsExactlyOnTheLimit(t *testing.T) {
	payload, err := json.Marshal(calendar.Event{
		Summary:     strings.Repeat("s", MaxSummary),
		Description: strings.Repeat("d", MaxDescription),
		Location:    strings.Repeat("l", MaxLocation),
		StartAt:     "2026-03-04T09:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	if _, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, payload); err != nil {
		t.Fatalf("Handle at the limit: %v", err)
	}
}

// The read tool is bounded: a window is required and a result count is capped, so a
// read cannot become a way to pull an account's whole history.
func TestListEventsRequiresABoundedWindow(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    string
	}{
		"no window":        {payload: `{}`, want: "time_min is required"},
		"only start":       {payload: `{"time_min":"2026-03-04T00:00:00Z"}`, want: "time_max is required"},
		"inverted window":  {payload: `{"time_min":"2026-03-05T00:00:00Z","time_max":"2026-03-04T00:00:00Z"}`, want: "time_max must be after time_min"},
		"unparseable max":  {payload: `{"time_min":"2026-03-04T00:00:00Z","time_max":"soon"}`, want: "time_max must be an RFC 3339"},
		"negative results": {payload: `{"time_min":"2026-03-04T00:00:00Z","time_max":"2026-03-05T00:00:00Z","max_results":-1}`, want: "max_results must not be negative"},
	}

	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tools.Handle(t.Context(), user, calendar.ListEventsTool, json.RawMessage(test.payload))
			if err == nil {
				t.Fatal("Handle succeeded, want a refusal")
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestListEventsSendsTheBoundedQuery(t *testing.T) {
	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	payload := `{"time_min":"2026-03-04T00:00:00Z","time_max":"2026-03-05T00:00:00Z","max_results":9999,"query":"standup","single_events":true}`

	if _, err := tools.Handle(t.Context(), user, calendar.ListEventsTool, json.RawMessage(payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if api.query.MaxResults != calendar.MaxListResults {
		t.Errorf("MaxResults = %d, want it capped at %d", api.query.MaxResults, calendar.MaxListResults)
	}

	if api.query.SearchTerm != "standup" || !api.query.SingleEvents {
		t.Errorf("query = %+v, want the approved term and expansion", api.query)
	}

	if api.query.CalendarID != "" {
		t.Errorf("CalendarID = %q, want the provider's default when none was approved", api.query.CalendarID)
	}
}

// A default page size is applied rather than refusing, because asking for "the events
// that week" without a count is the normal way to phrase it.
func TestListEventsAppliesADefaultPageSize(t *testing.T) {
	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	payload := `{"time_min":"2026-03-04T00:00:00Z","time_max":"2026-03-05T00:00:00Z"}`

	if _, err := tools.Handle(t.Context(), user, calendar.ListEventsTool, json.RawMessage(payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if api.query.MaxResults != calendar.DefaultListResults {
		t.Errorf("MaxResults = %d, want %d", api.query.MaxResults, calendar.DefaultListResults)
	}
}

// An empty result is an answer. A nil slice would encode as null and read downstream
// as the tool having failed to produce a list.
func TestListEventsReportsAnEmptyResultAsAnEmptyList(t *testing.T) {
	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	payload := `{"time_min":"2026-03-04T00:00:00Z","time_max":"2026-03-05T00:00:00Z"}`

	result, err := tools.Handle(t.Context(), user, calendar.ListEventsTool, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(encoded), `"events":null`) {
		t.Errorf("result = %s, want an empty list rather than null", encoded)
	}

	if !strings.Contains(string(encoded), `"count":0`) {
		t.Errorf("result = %s, want the count reported", encoded)
	}
}

// Credentials are per user. A session for one account must never send another's token,
// because that is how an approval writes to the wrong calendar.
func TestTokensAreResolvedPerUser(t *testing.T) {
	api := &fakeAPI{}
	tokens := &fakeTokens{byUser: map[string]string{user: "token-alice", "user-2": "token-bob"}}
	tools := newTools(t, api, tokens)

	if _, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(approvedEvent)); err != nil {
		t.Fatalf("Handle for the first user: %v", err)
	}

	if api.token != "token-alice" {
		t.Errorf("token = %q, want the first user's own token", api.token)
	}

	if _, err := tools.Handle(t.Context(), "user-2", calendar.CreateEventTool, json.RawMessage(approvedEvent)); err != nil {
		t.Fatalf("Handle for the second user: %v", err)
	}

	if api.token != "token-bob" {
		t.Errorf("token = %q, want the second user's own token", api.token)
	}
}

// A disconnected calendar must fail before the provider is called, or the provider
// would receive an empty credential and report a confusing authorisation error.
func TestCreateEventRefusesWhenTheCalendarIsNotConnected(t *testing.T) {
	api := &fakeAPI{}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{}})

	_, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(approvedEvent))
	if err == nil {
		t.Fatal("Handle succeeded for a disconnected calendar")
	}

	if len(api.created) != 0 {
		t.Errorf("provider was called %d times, want 0", len(api.created))
	}
}

// A provider that accepts the call and returns nothing leaves the user unable to tell
// whether anything happened, so it is reported rather than treated as success.
func TestCreateEventRefusesWhenTheProviderReturnsNothing(t *testing.T) {
	api := &fakeAPI{createNothing: true}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	_, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(approvedEvent))
	if err == nil {
		t.Fatal("Handle succeeded, want a refusal: nothing was returned to identify the event")
	}

	if !calendar.IsRefusal(err) {
		t.Errorf("error %v is not a refusal", err)
	}
}

// A provider failure is passed through untouched. The connector adds no interpretation
// of it, so the reason a user sees is the reason Google gave.
func TestCreateEventPassesAProviderFailureThrough(t *testing.T) {
	api := &fakeAPI{createErr: context.DeadlineExceeded}
	tools := newTools(t, api, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	_, err := tools.Handle(t.Context(), user, calendar.CreateEventTool, json.RawMessage(approvedEvent))
	if err == nil {
		t.Fatal("Handle succeeded, want the provider's failure")
	}

	if calendar.IsRefusal(err) {
		t.Error("a transport failure was reported as a refusal, which hides that the provider is unreachable")
	}

	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("error = %q, want the provider's reason preserved", err)
	}
}

func TestHandleRefusesAnUnknownTool(t *testing.T) {
	tools := newTools(t, &fakeAPI{}, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	_, err := tools.Handle(t.Context(), user, "calendar.delete_everything", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Handle succeeded for a tool the connector does not define")
	}
}

// Only the write tool may sit behind an approval, and only the read tool may be chosen
// without one. Getting this backwards would let a plan silently change a calendar.
func TestOnlyTheWriteToolIsMutating(t *testing.T) {
	tools := newTools(t, &fakeAPI{}, &fakeTokens{byUser: map[string]string{user: "token-abc"}})

	if tools.ReadOnly(calendar.ListEventsTool) != true {
		t.Error(calendar.ListEventsTool + " should be read-only")
	}

	if tools.ReadOnly(calendar.CreateEventTool) {
		t.Error(calendar.CreateEventTool + " changes a calendar, so it must not be read-only")
	}

	// An unresolvable name must not be assumed harmless.
	if tools.ReadOnly("calendar.nonexistent") {
		t.Error("an unknown tool was reported read-only, so it would slip past an approval")
	}

	names := tools.ToolNames()
	if len(names) != 2 {
		t.Fatalf("tools = %v, want exactly a read and a write", names)
	}
}

// The schema the model is shown and the set of keys the decoder accepts are two
// descriptions of the same tool. If they drift, the model is asked to produce something
// this code will refuse, or a field is accepted that the user was never shown.
func TestSchemaPropertiesMatchTheAcceptedArguments(t *testing.T) {
	cases := map[string]struct {
		schema  json.RawMessage
		tool    string
		decoded func(json.RawMessage) error
	}{
		"create": {
			schema: calendar.CreateEventSchema(),
			tool:   calendar.CreateEventTool,
			decoded: func(raw json.RawMessage) error {
				_, err := calendar.EventFromArguments(raw)

				return err
			},
		},
		"list": {
			schema: calendar.ListEventsSchema(),
			tool:   calendar.ListEventsTool,
			decoded: func(raw json.RawMessage) error {
				_, err := calendar.QueryFromArguments(raw)

				return err
			},
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			var schema map[string]any

			if err := json.Unmarshal(test.schema, &schema); err != nil {
				t.Fatalf("decode schema: %v", err)
			}

			properties, _ := schema["properties"].(map[string]any)

			required := map[string]bool{}

			if listed, ok := schema["required"].([]any); ok {
				for _, field := range listed {
					name, _ := field.(string)
					required[name] = true
				}
			}

			if len(properties) == 0 {
				t.Fatal("schema describes no properties")
			}

			// Every advertised property must be accepted by the decoder, so the model
			// is never shown a field that is then refused.
			for property := range properties {
				payload := minimalPayloadFor(property)

				if err := test.decoded(json.RawMessage(payload)); err != nil && strings.Contains(err.Error(), "is not part of this tool") {
					t.Errorf("schema advertises %q but the decoder refuses it: %v", property, err)
				}
			}

			// Every required field must be advertised as required.
			for field := range required {
				if _, ok := properties[field]; !ok {
					t.Errorf("schema requires %q but does not describe it", field)
				}
			}

			if len(required) == 0 {
				t.Error("schema names no required field")
			}
		})
	}
}

// minimalPayloadFor builds the smallest payload that mentions a field, so a field can
// be tested in isolation.
func minimalPayloadFor(field string) string {
	payloads := map[string]string{
		"summary":       `{"summary":"Standup"}`,
		"start_at":      `{"start_at":"2026-03-04T09:00:00Z"}`,
		"end_at":        `{"end_at":"2026-03-04T09:00:00Z"}`,
		"timezone":      `{"timezone":"Europe/London"}`,
		"description":   `{"description":"detail"}`,
		"location":      `{"location":"Room 2"}`,
		"attendees":     `{"attendees":["sam@example.com"]}`,
		"calendar_id":   `{"calendar_id":"primary"}`,
		"time_min":      `{"time_min":"2026-03-04T00:00:00Z"}`,
		"time_max":      `{"time_max":"2026-03-05T00:00:00Z"}`,
		"max_results":   `{"max_results":10}`,
		"query":         `{"query":"standup"}`,
		"single_events": `{"single_events":true}`,
	}

	return payloads[field]
}

// The schema the model is shown has to forbid the fields the decoder refuses, or the
// model is being asked to do something this code will reject.
func TestSchemasForbidUnknownProperties(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"create": calendar.CreateEventSchema(),
		"list":   calendar.ListEventsSchema(),
	} {
		var decoded map[string]any

		if err := json.Unmarshal(schema, &decoded); err != nil {
			t.Fatalf("%s schema is not valid JSON: %v", name, err)
		}

		if allowed, ok := decoded["additionalProperties"].(bool); !ok || allowed {
			t.Errorf("%s schema does not set additionalProperties false", name)
		}

		if _, ok := decoded["required"]; !ok {
			t.Errorf("%s schema names no required field", name)
		}
	}
}

// The declared limits and the validator's limits are the same numbers. A schema
// advertising 500 while the validator refuses 400 would make the model generate
// payloads that are refused after the user has approved them.
func TestSchemaLimitsMatchTheValidator(t *testing.T) {
	var create map[string]any

	if err := json.Unmarshal(calendar.CreateEventSchema(), &create); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	properties, ok := create["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}

	for field, want := range map[string]int{
		"summary":     calendar.MaxSummaryLength,
		"description": calendar.MaxDescriptionLength,
		"location":    calendar.MaxLocationLength,
	} {
		property, ok := properties[field].(map[string]any)
		if !ok {
			t.Errorf("schema has no %s property", field)
			continue
		}

		got, _ := property["maxLength"].(float64)

		if int(got) != want {
			t.Errorf("%s maxLength = %v, want %d", field, property["maxLength"], want)
		}
	}

	var list map[string]any

	if err := json.Unmarshal(calendar.ListEventsSchema(), &list); err != nil {
		t.Fatalf("decode list schema: %v", err)
	}

	listProperties, _ := list["properties"].(map[string]any)

	maxResults, _ := listProperties["max_results"].(map[string]any)
	if got, _ := maxResults["maximum"].(float64); int(got) != calendar.MaxListResults {
		t.Errorf("max_results maximum = %v, want %d", maxResults["maximum"], calendar.MaxListResults)
	}
}
