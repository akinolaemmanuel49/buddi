package googlecalendar_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
)

// Google's own 400 for an event whose times are unusable. The reason lives in
// errors[].reason, not in message, which is why this used to surface as a bare
// "Bad Request".
func TestCreateEventReportsGooglesReason(t *testing.T) {
	google := &fakeGoogle{
		status: http.StatusBadRequest,
		response: `{"error":{"code":400,"message":"Bad Request",` +
			`"errors":[{"domain":"global","reason":"badRequest",` +
			`"message":"End time must be after start time."}],"status":"INVALID_ARGUMENT"}}`,
	}

	client := newClient(t, google)

	_, err := client.CreateEvent(t.Context(), "token", calendar.Event{
		Summary: "Dentist",
		StartAt: "2026-10-15T15:00:00Z",
	})
	if err == nil {
		t.Fatal("CreateEvent accepted a 400")
	}

	text := err.Error()

	for _, want := range []string{
		"VERY LOUD BUG",
		"End time must be after start time.",
		"INVALID_ARGUMENT",
		"badRequest",
		"400",
		// The raw body, so nothing is lost even if the parsing is wrong.
		`"error"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("error does not contain %q:\n%s", want, text)
		}
	}
}

// A 400 with no parseable body must still say the body was empty rather than leaving
// a dangling colon.
func TestCreateEventReportsAnUnparseableErrorBody(t *testing.T) {
	google := &fakeGoogle{status: http.StatusBadRequest, response: "<html>gateway timeout</html>"}

	client := newClient(t, google)

	_, err := client.CreateEvent(t.Context(), "token", calendar.Event{
		Summary: "Dentist",
		StartAt: "2026-10-15T15:00:00Z",
	})
	if err == nil {
		t.Fatal("CreateEvent accepted a 400")
	}

	if !strings.Contains(err.Error(), "gateway timeout") {
		t.Errorf("error dropped the body Google sent:\n%s", err)
	}
}

// 401 and 403 keep the advice to reconnect, since that is the one thing the user can
// act on, and it still carries the raw body.
func TestAnAuthFailureStillAdvisesReconnecting(t *testing.T) {
	google := &fakeGoogle{
		status:   http.StatusUnauthorized,
		response: `{"error":{"code":401,"message":"Invalid Credentials"}}`,
	}

	client := newClient(t, google)

	_, err := client.CreateEvent(t.Context(), "expired-token", calendar.Event{
		Summary: "Dentist",
		StartAt: "2026-10-15T15:00:00Z",
	})
	if err == nil {
		t.Fatal("CreateEvent accepted a 401")
	}

	if !strings.Contains(err.Error(), "Reconnect") {
		t.Errorf("error does not advise reconnecting:\n%s", err)
	}
}
