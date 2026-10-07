package agent_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
)

// A 3pm appointment reached the calendar as 4pm.
//
// The plan's instant was not the problem — 15:00 in London is unambiguous once you know
// the zone. The encoder was flattening it to 15:00Z and declaring timeZone "UTC", and
// Google renders an instant in the calendar's own zone. 15:00Z in London during BST is
// 16:00, so the appointment was an hour out while looking correct in the payload.
func TestAnEventIsWrittenInTheUsersOwnZone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Three in the afternoon in London, on a day BST is in force.
	start := time.Date(2026, time.October, 9, 14, 0, 0, 0, time.UTC)

	plan := &planner.Plan{
		Title:    "Dentist Appointment",
		DueAt:    &start,
		TimeZone: "Europe/London",
	}

	encoded, err := agent.EncodeCalendarArguments(plan)
	if err != nil {
		t.Fatalf("EncodeCalendarArguments: %v", err)
	}

	var sent struct {
		Summary  string `json:"summary"`
		StartAt  string `json:"start_at"`
		TimeZone string `json:"timezone"`
	}

	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}

	if sent.TimeZone != "Europe/London" {
		t.Errorf("timezone = %q, want Europe/London: Google renders the instant in the "+
			"calendar's own zone, so this has to be stated", sent.TimeZone)
	}

	// The local wall-clock reading has to be 15:00, not 14:00 and not 16:00.
	parsed, err := time.Parse(time.RFC3339, sent.StartAt)
	if err != nil {
		t.Fatalf("parse start_at %q: %v", sent.StartAt, err)
	}

	if got := parsed.In(loc).Format("15:04"); got != "15:00" {
		t.Errorf("start_at = %q, which reads as %s in London, want 15:00",
			sent.StartAt, got)
	}

	// The instant itself must not move, or the appointment lands on a different day.
	if !parsed.Equal(start) {
		t.Errorf("start_at = %q, which is %s, want the instant %s",
			sent.StartAt, parsed, start)
	}
}

// An event with a known zone must not be shifted by the offset. The bug is only fixed if
// the instant is preserved and the zone is declared; changing one to compensate for the
// other would fix the display and break the day.
func TestWritingInALocalZoneDoesNotShiftTheInstant(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Midnight on the 9th in London is 23:00 on the 8th in UTC. Flattening this to UTC
	// would put the event on the wrong day entirely.
	local := time.Date(2026, time.October, 9, 0, 0, 0, 0, loc)
	instant := local.UTC()

	plan := &planner.Plan{
		Title:    "Flight",
		DueAt:    &instant,
		TimeZone: "Europe/London",
	}

	encoded, err := agent.EncodeCalendarArguments(plan)
	if err != nil {
		t.Fatalf("EncodeCalendarArguments: %v", err)
	}

	var sent struct {
		StartAt string `json:"start_at"`
	}

	if err := json.Unmarshal(encoded, &sent); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}

	parsed, err := time.Parse(time.RFC3339, sent.StartAt)
	if err != nil {
		t.Fatalf("parse start_at %q: %v", sent.StartAt, err)
	}

	if !parsed.Equal(instant) {
		t.Errorf("start_at = %q, want the instant %s unchanged", sent.StartAt, instant)
	}

	if got := parsed.In(loc).Format("2006-01-02 15:04"); got != "2026-10-09 00:00" {
		t.Errorf("start_at reads as %s in London, want 2026-10-09 00:00", got)
	}
}

// A plan with no recorded zone must not claim to be UTC. Claiming a zone that is not
// known to be true is the original bug, and omitting it lets Google use the calendar's
// own — which is at least what the user sees everywhere else.
func TestAPlanWithNoZoneDoesNotClaimUTC(t *testing.T) {
	start := time.Date(2026, time.October, 9, 15, 0, 0, 0, time.UTC)

	plan := &planner.Plan{Title: "Dentist", DueAt: &start}

	encoded, err := agent.EncodeCalendarArguments(plan)
	if err != nil {
		t.Fatalf("EncodeCalendarArguments: %v", err)
	}

	if strings.Contains(string(encoded), `"timezone"`) {
		t.Errorf("arguments = %s, want no timezone claimed for a plan that has none", encoded)
	}
}

// An unknown zone name is not written into the event. It reaches here from a browser, so
// the failure mode is a renamed zone rather than a hostile value, and sending an invalid
// name would have Google reject the write.
func TestAnUnknownZoneIsNotWrittenIntoTheEvent(t *testing.T) {
	start := time.Date(2026, time.October, 9, 15, 0, 0, 0, time.UTC)

	plan := &planner.Plan{Title: "Dentist", DueAt: &start, TimeZone: "Mars/Olympus_Mons"}

	encoded, err := agent.EncodeCalendarArguments(plan)
	if err != nil {
		t.Fatalf("EncodeCalendarArguments: %v", err)
	}

	if strings.Contains(string(encoded), "Olympus") {
		t.Errorf("arguments = %s, want the unknown zone omitted", encoded)
	}
}
