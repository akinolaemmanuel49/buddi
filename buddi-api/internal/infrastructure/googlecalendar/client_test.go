package googlecalendar_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/googlecalendar"
)

// capturedRequest is what a fake Google recorded.
type capturedRequest struct {
	method string
	path   string

	// rawPath is the path as it appeared on the wire, before the server decoded it. The
	// decoded form is misleading here: an escaped slash arrives as a slash, so a
	// client that failed to escape would be indistinguishable from one that escaped
	// correctly once the server has had its way with the request.
	rawPath string

	query url.Values
	body  string
	auth  string
}

// fakeGoogle stands in for the API and answers with whatever it is told to.
type fakeGoogle struct {
	requests []capturedRequest

	status      int
	response    string
	contentType string
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.requests = append(f.requests, capturedRequest{
		method:  r.Method,
		path:    r.URL.Path,
		rawPath: r.URL.EscapedPath(),
		query:   r.URL.Query(),
		body:    string(body),
		auth:    r.Header.Get("Authorization"),
	})

	status := f.status
	if status == 0 {
		status = http.StatusOK
	}

	w.Header().Set("Content-Type", f.contentType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, f.response)
}

func newClient(t *testing.T, google *fakeGoogle) *googlecalendar.Client {
	t.Helper()

	server := httptest.NewServer(google)
	t.Cleanup(server.Close)

	client, err := googlecalendar.New(googlecalendar.Options{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client
}

// The request Google receives must contain the event and nothing else. This is the
// outbound half of §10.2: the connector refuses note content on the way in, and this
// asserts that what goes out is only what the payload described.
func TestCreateEventSendsOnlyTheEventFields(t *testing.T) {
	google := &fakeGoogle{response: `{"id":"evt-1","htmlLink":"https://example.com/e/evt-1","summary":"Deploy freeze","start":{"dateTime":"2026-03-04T14:00:00Z"}}`}

	client := newClient(t, google)

	event := calendar.Event{
		Summary:     "Deploy freeze",
		Description: "Announced in #release",
		Location:    "Room 2",
		StartAt:     "2026-03-04T14:00:00Z",
		EndAt:       "2026-03-04T15:00:00Z",
		TimeZone:    "Europe/London",
		Attendees:   []string{"sam@example.com"},
	}

	if _, err := client.CreateEvent(t.Context(), "ya29.alice", event); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	if len(google.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(google.requests))
	}

	request := google.requests[0]

	if request.method != http.MethodPost {
		t.Errorf("method = %s, want POST", request.method)
	}

	if request.path != "/calendars/primary/events" {
		t.Errorf("path = %q, want the primary calendar's events", request.path)
	}

	if request.auth != "Bearer ya29.alice" {
		t.Errorf("Authorization = %q, want the user's own token", request.auth)
	}

	var body map[string]any

	if err := json.Unmarshal([]byte(request.body), &body); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, request.body)
	}

	allowed := map[string]bool{
		"summary":     true,
		"description": true,
		"location":    true,
		"start":       true,
		"end":         true,
		"attendees":   true,
	}

	for key := range body {
		if !allowed[key] {
			t.Errorf("request carries %q, which is not part of an approved event", key)
		}
	}

	if body["summary"] != "Deploy freeze" {
		t.Errorf("summary = %v, want the approved one", body["summary"])
	}

	start, _ := body["start"].(map[string]any)
	// The zone is written as an explicit offset rather than "Z". Same instant, but it
	// is the form every working payload uses, and its absence is what produced an
	// unexplained 400 from Google.
	if start["dateTime"] != "2026-03-04T14:00:00+00:00" {
		t.Errorf("start.dateTime = %v, want the approved instant with an explicit offset",
			start["dateTime"])
	}

	if start["timeZone"] != "Europe/London" {
		t.Errorf("start.timeZone = %v, want the approved zone", start["timeZone"])
	}

	attendees, _ := body["attendees"].([]any)
	if len(attendees) != 1 {
		t.Fatalf("attendees = %v, want the one approved address", body["attendees"])
	}

	first, _ := attendees[0].(map[string]any)
	if first["email"] != "sam@example.com" {
		t.Errorf("attendee email = %v, want the approved address", first["email"])
	}
}

// Google answers 400 "Missing end time" for an event with no end, so omitting it does
// not mean "no end" to the provider, it means "invalid". An hour is supplied and the
// event is marked unspecified, which is what a user who said "3pm, dentist" meant.
func TestCreateEventDefaultsAnAbsentEnd(t *testing.T) {
	google := &fakeGoogle{response: `{"id":"evt-1","summary":"Dentist","start":{"dateTime":"2026-03-04T15:00:00Z"},"end":{"dateTime":"2026-03-04T16:00:00Z"}}`}

	client := newClient(t, google)

	if _, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary: "Dentist",
		StartAt: "2026-03-04T15:00:00Z",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	var body map[string]any

	if err := json.Unmarshal([]byte(google.requests[0].body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	end, present := body["end"].(map[string]any)
	if !present {
		t.Fatalf("request carries no end, which Google rejects: %v", body)
	}

	if end["dateTime"] != "2026-03-04T16:00:00+00:00" {
		t.Errorf("end = %v, want an hour after the start", end["dateTime"])
	}

	// Not sent: the connector invents an hour, so asserting the end is unspecified
	// would claim something untrue about the payload it accompanies. It was also the one
	// variable changed alongside the end, which is what turned Google's specific
	// "Missing end time" into an unexplained "Bad Request".
	if _, present := body["endTimeUnspecified"]; present {
		t.Errorf("endTimeUnspecified = %v, want it omitted", body["endTimeUnspecified"])
	}
}

// With no zone named by the caller the payload still states one, because an event sent
// without a timeZone is what Google answers with an unexplained 400.
func TestCreateEventStatesATimeZoneEvenWhenNoneWasGiven(t *testing.T) {
	google := &fakeGoogle{response: `{"id":"e","summary":"Dentist","start":{"dateTime":"2026-10-15T15:00:00Z"},"end":{"dateTime":"2026-10-15T16:00:00Z"}}`}

	client := newClient(t, google)

	if _, err := client.CreateEvent(t.Context(), "token", calendar.Event{
		Summary: "Dentist",
		StartAt: "2026-10-15T15:00:00Z",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	var body map[string]any

	if err := json.Unmarshal([]byte(google.requests[0].body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	start, _ := body["start"].(map[string]any)
	if start["timeZone"] != "UTC" {
		t.Errorf("start.timeZone = %v, want UTC", start["timeZone"])
	}

	end, _ := body["end"].(map[string]any)
	if end["timeZone"] != "UTC" {
		t.Errorf("end.timeZone = %v, want UTC", end["timeZone"])
	}
}

// An end the request actually stated must be used as given, and must not be marked
// unspecified: the user did say when it ends.
func TestCreateEventKeepsAStatedEnd(t *testing.T) {
	google := &fakeGoogle{response: `{"id":"evt-1","summary":"Review","start":{"dateTime":"2026-03-04T10:00:00Z"},"end":{"dateTime":"2026-03-04T10:30:00Z"}}`}

	client := newClient(t, google)

	if _, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary: "Review",
		StartAt: "2026-03-04T10:00:00Z",
		EndAt:   "2026-03-04T10:30:00Z",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	var body map[string]any

	if err := json.Unmarshal([]byte(google.requests[0].body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	end, _ := body["end"].(map[string]any)
	if end["dateTime"] != "2026-03-04T10:30:00+00:00" {
		t.Errorf("end = %v, want the stated end", end["dateTime"])
	}
}

// With neither a start nor an end there is nothing to invent an hour from, and a 400
// from Google naming a field the caller never set is worse than saying so here.
func TestCreateEventRejectsAnEventWithNoStart(t *testing.T) {
	google := &fakeGoogle{response: `{}`}

	client := newClient(t, google)

	if _, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary: "Nowhere",
	}); err == nil {
		t.Error("CreateEvent accepted an event with no start")
	}
}

// A start that is not RFC3339 cannot have an hour added to it, so it is reported
// rather than silently sent on to Google.
func TestCreateEventRejectsAnUnparseableStart(t *testing.T) {
	google := &fakeGoogle{response: `{}`}

	client := newClient(t, google)

	if _, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary: "Bad",
		StartAt: "next tuesday",
	}); err == nil {
		t.Error("CreateEvent accepted a start that is not a timestamp")
	}
}

// Google's identifier for the user's own calendar is "primary". Sending an empty
// identifier would address a calendar that does not exist.
func TestCreateEventDefaultsToThePrimaryCalendar(t *testing.T) {
	google := &fakeGoogle{response: `{"id":"evt-1","summary":"Standup","start":{"dateTime":"2026-03-04T09:00:00Z"}}`}

	client := newClient(t, google)

	_, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary:    "Standup",
		StartAt:    "2026-03-04T09:00:00Z",
		CalendarID: "  ",
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	if got := google.requests[0].path; got != "/calendars/primary/events" {
		t.Errorf("path = %q, want the primary calendar", got)
	}
}

// A calendar identifier arrives in an approved payload, so it must not be able to
// address somewhere else. Without escaping, one beginning with a slash would change
// which endpoint is written to.
func TestACalendarIdentifierCannotRedirectTheRequest(t *testing.T) {
	cases := map[string]struct {
		id      string
		wantURL string
	}{
		"slash is escaped": {
			id:      "team/releases",
			wantURL: "/calendars/team%2Freleases/events",
		},
		"traversal is escaped": {
			id:      "../otheruser/calendar",
			wantURL: "/calendars/..%2Fotheruser%2Fcalendar/events",
		},
		"query cannot be injected": {
			id:      "primary?a=b",
			wantURL: "",
		},
		"fragment cannot be injected": {
			id:      "primary#x",
			wantURL: "",
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			google := &fakeGoogle{response: `{"id":"evt-1","summary":"Standup","start":{"dateTime":"2026-03-04T09:00:00Z"}}`}

			client := newClient(t, google)

			_, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
				Summary:    "Standup",
				StartAt:    "2026-03-04T09:00:00Z",
				CalendarID: test.id,
			})

			if test.wantURL == "" {
				// A rejected identifier must not be sent at all. Escaping alone would
				// still put the attacker's string on the wire.
				if err == nil {
					t.Fatalf("CreateEvent accepted calendar_id %q", test.id)
				}

				if len(google.requests) != 0 {
					t.Errorf("a request was sent for a rejected identifier: %v", google.requests[0])
				}

				return
			}

			if err != nil {
				t.Fatalf("CreateEvent: %v", err)
			}

			// Asserted on the wire form: the server has decoded the path by now, so
			// an escaped slash and a bare one look the same here.
			if got := google.requests[0].rawPath; got != test.wantURL {
				t.Errorf("wire path = %q, want %q", got, test.wantURL)
			}
		})
	}
}

// A read is bounded on the wire as well as in the connector. A connector that validated
// a window and then dropped it would let a listing become a way to read everything.
func TestListEventsSendsTheBoundedWindow(t *testing.T) {
	google := &fakeGoogle{response: `{"items":[{"id":"evt-1","summary":"Standup","start":{"dateTime":"2026-03-04T09:00:00Z"}}]}`}

	client := newClient(t, google)

	min := time.Date(2026, time.March, 4, 0, 0, 0, 0, time.UTC)
	max := time.Date(2026, time.March, 5, 0, 0, 0, 0, time.UTC)

	events, err := client.ListEvents(t.Context(), "ya29.alice", calendar.ListQuery{
		TimeMin:      min,
		TimeMax:      max,
		MaxResults:   50,
		SearchTerm:   "standup",
		SingleEvents: true,
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	if len(events) != 1 || events[0].ID() != "evt-1" {
		t.Fatalf("events = %+v, want the one Google returned", events)
	}

	query := google.requests[0].query

	// The values are compared decoded. url.Values escapes the colons in a timestamp, so
	// comparing raw strings would assert the encoding rather than the window.
	for name, want := range map[string]string{
		"timeMin":      min.Format(time.RFC3339),
		"timeMax":      max.Format(time.RFC3339),
		"maxResults":   "50",
		"singleEvents": "true",
		"q":            "standup",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// An expired or revoked credential is the one failure a user can fix, so it is named as
// such. Reporting it the same way as a rate limit would send them to reconnect for no
// reason.
func TestARejectedCredentialIsReportedAsSomethingTheUserCanFix(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		google := &fakeGoogle{
			status:      status,
			response:    `{"error":{"code":` + http.StatusText(status) + `0,"message":"Invalid Credentials"}}`,
			contentType: "application/json",
		}

		client := newClient(t, google)

		_, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
			Summary: "Standup",
			StartAt: "2026-03-04T09:00:00Z",
		})
		if err == nil {
			t.Fatalf("CreateEvent succeeded with status %d", status)
		}

		if !strings.Contains(err.Error(), "Reconnect") {
			t.Errorf("status %d: error = %q, want it to say reconnecting is the fix", status, err)
		}
	}
}

// Other failures are the call's, not the credential's.
func TestOtherFailuresAreNotReportedAsAReconnect(t *testing.T) {
	google := &fakeGoogle{
		status:      http.StatusTooManyRequests,
		response:    `{"error":{"code":429,"message":"Rate Limit Exceeded"}}`,
		contentType: "application/json",
	}

	client := newClient(t, google)

	_, err := client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary: "Standup",
		StartAt: "2026-03-04T09:00:00Z",
	})
	if err == nil {
		t.Fatal("CreateEvent succeeded with a rate limit")
	}

	if strings.Contains(err.Error(), "Reconnect") {
		t.Errorf("error = %q, want a rate limit not reported as a credential problem", err)
	}

	if !strings.Contains(err.Error(), "Rate Limit Exceeded") {
		t.Errorf("error = %q, want Google's own reason preserved", err)
	}
}

// A credential must not end up in an error message. The token travels in a header, and
// Go's HTTP errors quote the request, so the failure path is where a secret leaks.
func TestNoCredentialAppearsInAnyError(t *testing.T) {
	secret := "ya29.super-secret-token"

	cases := map[string]*fakeGoogle{
		"unauthorised": {
			status:   http.StatusUnauthorized,
			response: `{"error":{"code":401,"message":"Invalid Credentials"}}`,
		},
		"unparseable response": {
			response: `{"id":` + secret + `}`,
		},
		"empty body": {response: ""},
	}

	for name, google := range cases {
		t.Run(name, func(t *testing.T) {
			client := newClient(t, google)

			_, err := client.CreateEvent(context.Background(), secret, calendar.Event{
				Summary: "Standup",
				StartAt: "2026-03-04T09:00:00Z",
			})

			if err == nil {
				return
			}

			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaked the credential: %q", err)
			}
		})
	}
}

// A transport failure is reported without the URL, which would carry the calendar
// identifier into any log recording it.
func TestATransportFailureDoesNotQuoteTheCalendarIdentifier(t *testing.T) {
	client, err := googlecalendar.New(googlecalendar.Options{BaseURL: "http://127.0.0.1:1/calendar/v3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.CreateEvent(t.Context(), "ya29.alice", calendar.Event{
		Summary:    "Standup",
		StartAt:    "2026-03-04T09:00:00Z",
		CalendarID: "confidential-team-calendar",
	})
	if err == nil {
		t.Skip("nothing listening, but the call unexpectedly succeeded")
	}

	if strings.Contains(err.Error(), "confidential-team-calendar") {
		t.Errorf("error quoted the calendar identifier: %q", err)
	}
}
