// Package googlecalendar is the Google Calendar REST implementation of calendar.API.
//
// It is the only place that knows Google exists. Everything above it deals in
// calendar.Event, which is what makes the connector's tests meaningful: they assert
// what Google would receive without needing an account, and this file's own tests
// assert what Google is actually asked for without needing a network.
package googlecalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
)

// DefaultBaseURL is Google's calendar endpoint.
const DefaultBaseURL = "https://www.googleapis.com/calendar/v3"

// defaultTimeout bounds a single call. A user waiting on an approval should get an
// answer rather than a spinner, and a hung connection to a third party is not
// something to leave open indefinitely.
const defaultTimeout = 20 * time.Second

// maxResponseBytes caps a response.
//
// Google's error bodies can be large and a listing is the largest thing this reads.
// The cap exists so a provider answering with something enormous cannot exhaust this
// process's memory, and it is generous enough that a legitimate page never reaches it.
const maxResponseBytes = 8 << 20

// primaryCalendar is Google's identifier for the user's own calendar.
const primaryCalendar = "primary"

// defaultEventDuration is how long an event with no stated end lasts.
//
// An hour because that is the length people mean by default and what every calendar
// client falls back to; it is only ever sent when the request gave no end.
const defaultEventDuration = time.Hour

// Client calls Google Calendar on one user's behalf.
type Client struct {
	http    *http.Client
	baseURL string
}

// Options configures a Client.
type Options struct {
	// BaseURL overrides the API endpoint. Set only in tests; a production client
	// reaches Google.
	BaseURL string
	// Timeout bounds a single call.
	Timeout time.Duration
	// HTTPClient replaces the underlying client.
	HTTPClient *http.Client
}

// New builds a client.
func New(opts Options) (*Client, error) {
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("googlecalendar: %q is not a usable base URL", baseURL)
	}

	httpClient := opts.HTTPClient

	if httpClient == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}

		httpClient = &http.Client{Timeout: timeout}
	}

	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

// CreateEvent implements calendar.API.
//
// The event is translated field by field into Google's shape rather than being
// marshalled straight from calendar.Event. The translation is explicit so a change to
// our own struct cannot silently add a field to what Google receives.
func (c *Client) CreateEvent(ctx context.Context, accessToken string, event calendar.Event) (*calendar.Event, error) {
	body := map[string]any{
		"summary": event.Summary,
	}

	if event.Description != "" {
		body["description"] = event.Description
	}

	if event.Location != "" {
		body["location"] = event.Location
	}

	if len(event.Attendees) > 0 {
		attendees := make([]map[string]string, 0, len(event.Attendees))

		for _, attendee := range event.Attendees {
			attendees = append(attendees, map[string]string{"email": strings.TrimSpace(attendee)})
		}

		body["attendees"] = attendees
	}

	start, err := calendarInstant(event.StartAt, event.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("googlecalendar: start: %w", err)
	}

	body["start"] = start

	// Google refuses an event with no end: it answers 400 "Missing end time", so an
	// omitted end is not an acceptable way to say "this is a point in time". A start
	// with no stated end is therefore sent as an hour long.
	switch {
	case event.EndAt != "":
		end, err := calendarInstant(event.EndAt, event.TimeZone)
		if err != nil {
			return nil, fmt.Errorf("googlecalendar: end: %w", err)
		}

		body["end"] = end

	case event.StartAt != "":
		parsed, err := time.Parse(time.RFC3339, event.StartAt)
		if err != nil {
			return nil, fmt.Errorf("googlecalendar: start %q is not an RFC3339 timestamp: %w",
				event.StartAt, err)
		}

		end, err := calendarInstant(
			parsed.Add(defaultEventDuration).Format(time.RFC3339), event.TimeZone)
		if err != nil {
			return nil, fmt.Errorf("googlecalendar: derived end: %w", err)
		}

		body["end"] = end

	default:
		// Neither end nor start. Google would reject this too, and saying so here is
		// more useful than a 400 naming a field the caller never set.
		return nil, errors.New("googlecalendar: an event needs a start time")
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("googlecalendar: could not encode the event: %w", err)
	}

	target, err := c.eventsURL(event.CalendarID)
	if err != nil {
		return nil, err
	}

	var response eventResponse

	if err := c.do(ctx, accessToken, http.MethodPost, target, encoded, &response); err != nil {
		return nil, err
	}

	created := &calendar.Event{
		Summary:  response.Summary,
		StartAt:  response.Start.Start(),
		EndAt:    response.End.Start(),
		Location: response.Location,
	}

	created.SetIdentity(response.ID, response.Link)

	return created, nil
}

// ListEvents implements calendar.API.
func (c *Client) ListEvents(ctx context.Context, accessToken string, query calendar.ListQuery) ([]calendar.Event, error) {
	target, err := c.eventsURL(query.CalendarID)
	if err != nil {
		return nil, err
	}

	parameters := url.Values{}
	parameters.Set("timeMin", query.TimeMin.UTC().Format(time.RFC3339))
	parameters.Set("timeMax", query.TimeMax.UTC().Format(time.RFC3339))

	if query.MaxResults > 0 {
		parameters.Set("maxResults", strconv.Itoa(query.MaxResults))
	}

	if query.SearchTerm != "" {
		parameters.Set("q", query.SearchTerm)
	}

	if query.SingleEvents {
		parameters.Set("singleEvents", "true")
	}

	target += "?" + parameters.Encode()

	var response listResponse

	if err := c.do(ctx, accessToken, http.MethodGet, target, nil, &response); err != nil {
		return nil, err
	}

	events := make([]calendar.Event, 0, len(response.Items))

	for _, item := range response.Items {
		event := calendar.Event{
			Summary:     item.Summary,
			Description: item.Description,
			Location:    item.Location,
			StartAt:     item.Start.Start(),
			EndAt:       item.End.Start(),
		}

		event.SetIdentity(item.ID, item.Link)

		events = append(events, event)
	}

	return events, nil
}

// eventsURL builds the events endpoint for a calendar.
//
// The identifier is escaped as a path segment. Without that, a calendar id beginning
// with a slash would change which endpoint is addressed, so an approved payload would
// be able to redirect the write.
func (c *Client) eventsURL(calendarID string) (string, error) {
	id := strings.TrimSpace(calendarID)
	if id == "" {
		id = primaryCalendar
	}

	if strings.ContainsAny(id, "?#\n\r") {
		return "", calendar.Refused("calendar_id is not a valid calendar identifier")
	}

	return c.baseURL + "/calendars/" + url.PathEscape(id) + "/events", nil
}

// eventTime is Google's start or end object. Its two shapes are kept apart rather than
// collapsed into one: an all-day event states a date, a timed one states a date and
// time, and reading the wrong field yields a zero time rather than an error.
type eventTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
}

// Start returns whichever of the two Google sent, so a caller never has to know which
// kind of event it is looking at.
func (t eventTime) Start() string {
	if t.DateTime != "" {
		return t.DateTime
	}

	return t.Date
}

// eventResponse is what Google returns for one event.
type eventResponse struct {
	ID          string    `json:"id"`
	Link        string    `json:"htmlLink"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	Start       eventTime `json:"start"`
	End         eventTime `json:"end"`
}

// listResponse is what Google returns for a page of events.
type listResponse struct {
	Items []eventResponse `json:"items"`
}

// calendarInstant renders one end of an event.
//
// It differs from a plain RFC 3339 string in two ways, both about Google's parser
// rather than about correctness. The zone is written as an explicit numeric offset
// instead of "Z", and a timeZone is always present rather than omitted when unknown.
//
// Both forms are valid RFC 3339 and the documentation allows either, but an event sent
// without them is what produced an unexplained 400 "Bad Request", while every payload
// that works states an offset and a zone. "UTC" is used when the caller named none,
// which asserts what the timestamp already means rather than guessing the user's zone.
func calendarInstant(at string, zone string) (map[string]string, error) {
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return nil, fmt.Errorf("%q is not an RFC3339 timestamp: %w", at, err)
	}

	if strings.TrimSpace(zone) == "" {
		zone = "UTC"
	}

	return map[string]string{
		// -07:00 is the layout that renders a UTC instant as +00:00, which is the
		// explicit-offset form Google examples use.
		"dateTime": parsed.Format("2006-01-02T15:04:05-07:00"),
		"timeZone": zone,
	}, nil
}

// googleErrorBody is the shape Google uses for a failure.
//
// It decodes more than error.message on purpose. Google puts the actionable part in
// several different places depending on the failure: errors[].reason carries the field
// that was wrong, status carries the canonical name, and message is often nothing more
// than "Bad Request". Decoding only message produced exactly that, and a connector
// reporting "Bad Request" tells a reader nothing they can act on.
type googleErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`

		Errors []struct {
			Domain  string `json:"domain"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// loudBugPrefix marks a failure this connector could not explain.
//
// Deliberately ugly. A provider error that reaches this code has already been reduced
// to whatever could be parsed, and the part that was dropped is usually the part that
// mattered. So anything that gets this far is prefixed loudly enough that it cannot be
// mistaken for a routine 400 and skimmed past, which is how "Missing end time" sat
// behind a wall of otherwise reasonable logs.
const loudBugPrefix = "VERY LOUD BUG"

// maxReportedBody bounds how much of Google's response is quoted back.
//
// Google's bodies are small, but this is unbounded data from a third party and the
// whole reason for reporting it is to be read.
const maxReportedBody = 8 << 10

// googleError turns a failure status into an error.
//
// The distinction that matters: 401 and 403 mean the credential is no good, which the
// user can fix by reconnecting, while everything else is a call that failed. Reporting
// both the same way would tell a user to reconnect for a rate limit.
//
// Everything Google said is preserved, including the raw body, because the parsed fields
// are a summary of it and the summary is what has been losing the reason.
func googleError(status int, raw []byte) error {
	var (
		decoded googleErrorBody
		parts   []string
	)

	summary := ""

	if err := json.Unmarshal(raw, &decoded); err == nil {
		if decoded.Error.Message != "" {
			summary = decoded.Error.Message
		}

		if decoded.Error.Status != "" {
			parts = append(parts, "status="+decoded.Error.Status)
		}

		for _, detail := range decoded.Error.Errors {
			// Both are kept: reason names the offending field and message explains it,
			// and which one Google populates varies by error.
			switch {
			case detail.Reason != "" && detail.Message != "":
				parts = append(parts, detail.Reason+": "+detail.Message)
			case detail.Reason != "":
				parts = append(parts, detail.Reason)
			case detail.Message != "":
				parts = append(parts, detail.Message)
			}
		}
	}

	detail := ""
	if len(parts) > 0 {
		detail = " [" + strings.Join(parts, "; ") + "]"
	}

	body := string(raw)
	if len(body) > maxReportedBody {
		body = body[:maxReportedBody] + "... (truncated)"
	}

	// Always included, even when the body parsed cleanly, so the record is complete
	// rather than a reconstruction that could itself be wrong.
	quoted := strings.TrimSpace(body)
	if quoted == "" {
		quoted = "(empty response body)"
	}

	advice := ""
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		advice = " Reconnect the calendar and try again."
	}

	return fmt.Errorf(
		"%s: googlecalendar: Google returned %d%s: %s.%s Raw response: %s",
		loudBugPrefix,
		status,
		detail,
		summary,
		advice,
		quoted,
	)
}

// do performs one request and decodes the response.
//
// The status is checked before decoding because a 4xx body has the error shape and a
// 2xx body has the result shape. Decoding first would mean one type for two very
// different answers.
func (c *Client) do(
	ctx context.Context,
	accessToken string,
	method string,
	url string,
	body []byte,
	result any,
) error {
	var reader io.Reader

	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("googlecalendar: could not build the request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("googlecalendar: request failed: %w", unwrapURLError(err))
	}
	defer res.Body.Close()

	// Bounded: the provider's response is unbounded data and this process holds
	// everything else alongside it.
	limited := io.LimitReader(res.Body, maxResponseBytes)

	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("googlecalendar: could not read the response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return googleError(res.StatusCode, raw)
	}

	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("googlecalendar: the response was not what the API documents: %w", err)
	}

	return nil
}

// unwrapURLError strips the URL from a transport error.
//
// net/http's error text includes the request URL. Keeping it would put the calendar id
// into any log that records the error. The URL is not diagnostic here; the cause is.
func unwrapURLError(err error) error {
	var urlErr *url.Error

	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}

	return err
}
