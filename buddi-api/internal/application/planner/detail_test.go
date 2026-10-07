package planner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// The model fills title, due_at and location reliably and leaves description and
// attendees empty, every time, across repeated live probes. Prompt wording was tried
// twice; a retry complaint naming the missing person was tried as well and was worse,
// because a rejected plan dumped the whole request into the title and filled less.
//
// So these two are derived from the request in code, and only when the model left them
// empty. The invariant that matters is that neither can invent a name: everything comes
// from words the user typed, because this text lands in somebody's real calendar.

func dentistNow() time.Time {
	return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
}

func planFrom(t *testing.T, modelJSON, goal string) *planner.Outcome {
	t.Helper()

	generator := &fakeGenerator{responses: []planner.Response{ok(modelJSON)}}

	service := newService(t, generator, 0).WithClock(dentistNow)

	outcome, err := service.Plan(context.Background(), testUserID, goal)
	if err != nil {
		t.Fatalf("Plan(%q): %v", goal, err)
	}

	return outcome
}

func TestAPersonNamedInTheRequestBecomesAnAttendee(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"calendar_event","title":"Dentist Appointment","priority":"normal",`+
			`"due_at":"2026-10-09T15:00:00Z","steps":[]}`,
		"dentist appointment on Friday at 3pm with Dr Ada Okafor")

	plan := outcome.Plan

	if len(plan.Attendees) != 1 {
		t.Fatalf("attendees = %v, want the person the request named", plan.Attendees)
	}

	if plan.Attendees[0] != "Dr Ada Okafor" {
		t.Errorf("attendee = %q, want \"Dr Ada Okafor\"", plan.Attendees[0])
	}
}

// The extraction has to stop at the next clause, or "with Sam at the Italian place"
// yields the person and the restaurant.
func TestANamedPersonStopsAtTheNextClause(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"calendar_event","title":"Dinner","priority":"normal",`+
			`"due_at":"2026-10-09T20:00:00Z","steps":[]}`,
		"dinner with Sam at the Italian place on Brick Lane on Friday at 8pm")

	if len(outcome.Plan.Attendees) != 1 {
		t.Fatalf("attendees = %v, want one person", outcome.Plan.Attendees)
	}

	if got := outcome.Plan.Attendees[0]; got != "Sam" {
		t.Errorf("attendee = %q, want \"Sam\": the extraction ran past the clause boundary", got)
	}
}

// "dinner with me" is not an appointment with a person, and putting "me" in the
// attendee list would invite the user to email themselves.
func TestNoPersonIsInventedWhenNoneWasNamed(t *testing.T) {
	for _, goal := range []string{
		"dinner with me on Friday at 8pm",
		"lunch on Friday at 1pm",
		"haircut on Friday at 10am",
	} {
		outcome := planFrom(t,
			`{"intent":"calendar_event","title":"Something","priority":"normal",`+
				`"due_at":"2026-10-09T15:00:00Z","steps":[]}`,
			goal)

		if len(outcome.Plan.Attendees) != 0 {
			t.Errorf("Plan(%q) invented attendees %v", goal, outcome.Plan.Attendees)
		}
	}
}

// The model is the source when it answers. Deriving in code is a fallback, not an
// override, or a correct answer would be replaced by a cruder one.
func TestAModelSuppliedAttendeeIsNotOverwritten(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"calendar_event","title":"Coffee","priority":"normal",`+
			`"due_at":"2026-10-09T15:00:00Z","attendees":["Ana"],"steps":[]}`,
		"coffee with Ana on Friday at 3pm")

	if len(outcome.Plan.Attendees) != 1 || outcome.Plan.Attendees[0] != "Ana" {
		t.Errorf("attendees = %v, want the model's own answer", outcome.Plan.Attendees)
	}
}

// An appointment is not a sequence of actions. The schema no longer requires steps, but
// the model volunteers one often enough that the plan has to drop it.
func TestACalendarEventKeepsNoSteps(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"calendar_event","title":"Dentist Appointment","priority":"normal",`+
			`"due_at":"2026-10-09T15:00:00Z","steps":["Attend the dentist"]}`,
		"dentist appointment on Friday at 3pm")

	if len(outcome.Plan.Steps) != 0 {
		t.Errorf("a calendar event kept steps %v", outcome.Plan.Steps)
	}
}

// A task still needs its steps, so the dropping above must not have taken them with it.
func TestATaskKeepsItsSteps(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"task","title":"Buy milk","priority":"normal","steps":["Go to the shop"]}`,
		"buy milk")

	if len(outcome.Plan.Steps) != 1 {
		t.Errorf("a task lost its steps: %v", outcome.Plan.Steps)
	}
}

// A task gets no attendees, so the fallback must not put one there. "buy milk with Sam"
// is shopping together, not an event.
func TestATaskGainsNoAttendeeFromTheFallback(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"task","title":"Buy milk","priority":"normal","steps":["Go to the shop"]}`,
		"buy milk with Sam")

	if len(outcome.Plan.Attendees) != 0 {
		t.Errorf("a task gained attendees %v", outcome.Plan.Attendees)
	}
}

// Whatever fills these fields has to be reported, because a plan that was partly
// derived should not look exactly like one the model produced.
func TestADerivedAttendeeIsReported(t *testing.T) {
	outcome := planFrom(t,
		`{"intent":"calendar_event","title":"Dentist Appointment","priority":"normal",`+
			`"due_at":"2026-10-09T15:00:00Z","steps":[]}`,
		"dentist appointment on Friday at 3pm with Dr Ada Okafor")

	joined := strings.Join(outcome.Notes, "\n")

	if !strings.Contains(joined, "attendees") {
		t.Errorf("notes = %v, want the derived attendee reported", outcome.Notes)
	}
}

var _ = domain.PlanIntentTask
