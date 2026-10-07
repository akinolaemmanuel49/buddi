package planner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// The report that prompted the table: an errand with no time in it was being written to
// the user's calendar.
func TestAnErrandIsATaskNotACalendarEvent(t *testing.T) {
	for _, goal := range []string{
		"buy groceries",
		"Pick up the prescription",
		"do the laundry this evening",
		"stock up on cleaning supplies",
		"return the package",
	} {
		route := planner.RouteRequest(goal)

		if route.Intent != domain.PlanIntentTask {
			t.Errorf("RouteRequest(%q).Intent = %q, want task", goal, route.Intent)
		}

		if !route.Confident {
			t.Errorf("RouteRequest(%q) was not confident, so the model would decide", goal)
		}
	}
}

// An appointment with a time in it is an event.
func TestATimedAppointmentIsACalendarEvent(t *testing.T) {
	for _, goal := range []string{
		"dentist appointment on Friday at 3pm",
		"add a dentist appointment on Friday at 3pm",
		"book the haircut for tomorrow at 10am",
		"meeting with Sam on Thursday",
		"coffee with Ana on Tuesday morning",
	} {
		route := planner.RouteRequest(goal)

		if route.Intent != domain.PlanIntentCalendarEvent {
			t.Errorf("RouteRequest(%q).Intent = %q, want calendar_event", goal, route.Intent)
		}

		if route.NeedsClarification {
			t.Errorf("RouteRequest(%q) asked for clarification, but it names a time", goal)
		}
	}
}

// An appointment with no time in it is the case that matters. The one field that makes
// it an event is the one we would have to invent, and inventing it is how the wrong
// date reached the calendar in the first place.
func TestAnUntimedAppointmentAsksTheUserWhen(t *testing.T) {
	for _, goal := range []string{
		"I need to see the dentist",
		"book the haircut",
		"schedule a meeting with Sam",
	} {
		route := planner.RouteRequest(goal)

		if !route.NeedsClarification {
			t.Errorf("RouteRequest(%q) did not ask for the missing time", goal)
		}

		if route.Intent != domain.PlanIntentCalendarEvent {
			t.Errorf("RouteRequest(%q).Intent = %q, want calendar_event so the question is about the time",
				goal, route.Intent)
		}
	}
}

// "dentist at 3pm" names no weekday at all, so a check that only looked for weekday
// names would call it untimed and ask a pointless question.
func TestAClockReadingAloneCountsAsATime(t *testing.T) {
	for _, goal := range []string{
		"dentist at 3pm",
		"haircut at 15:00",
		"standup at 9am",
		"coffee at 4oclock",
	} {
		route := planner.RouteRequest(goal)

		if route.NeedsClarification {
			t.Errorf("RouteRequest(%q) asked when, but it states a clock time", goal)
		}
	}
}

// A number that is not a time must not read as one, or every request containing a
// quantity would be treated as scheduled.
func TestANumberThatIsNotAClockReadingIsNotATime(t *testing.T) {
	route := planner.RouteRequest("buy 2 milk")

	if route.NeedsClarification {
		t.Error("a quantity was read as a clock time")
	}
}

// Whole-word matching, because substring matching is how "may" became a month and
// "booking a table" became an errand.
func TestKeywordMatchingIsWholeWord(t *testing.T) {
	tests := []struct {
		goal string
		want domain.PlanIntent
		note string
	}{
		{"buy groceries", domain.PlanIntentTask, "groceries is listed"},
		{"maybe not", domain.PlanIntentTask, `contains "may" inside "maybe", so it matched nothing`},
		{"the bookstore", domain.PlanIntentTask, `"book" inside "bookstore" must not match`},
		{"book the dentist", domain.PlanIntentCalendarEvent, "book on its own is a keyword"},
	}

	for _, tt := range tests {
		if got := planner.RouteRequest(tt.goal).Intent; got != tt.want {
			t.Errorf("RouteRequest(%q).Intent = %q, want %q: %s", tt.goal, got, tt.want, tt.note)
		}
	}
}

// Errands are listed before appointments deliberately. "pick up the prescription" and
// "book the dentist" are both things a person does, but only one is an event.
func TestAnErrandMentioningABookingKeywordIsStillATask(t *testing.T) {
	route := planner.RouteRequest("pick up the book I ordered for the dentist")

	if route.Intent != domain.PlanIntentTask {
		t.Errorf("Intent = %q, want task: the errand rule is more specific", route.Intent)
	}
}

// A request nothing matched is genuinely ambiguous, and saying so is what lets the model
// decide without the table overriding every sentence it was not written for.
func TestAnUnmatchedRequestLeavesTheDecisionToTheModel(t *testing.T) {
	route := planner.RouteRequest("think about the state of the garden")

	if route.Confident {
		t.Error("an unmatched request was reported as confident")
	}

	if route.Matched != "" {
		t.Errorf("Matched = %q, want empty", route.Matched)
	}

	if route.NeedsClarification {
		t.Error("an unmatched request asked for clarification")
	}
}

// A clarification is a question, and a question nobody needs is its own failure: the
// user asked for something actionable and is being handed a follow-up instead.
//
// Exercised through Plan rather than by calling the check directly, because what
// matters is that the bad plan never reaches the caller.
func TestAModelThatAsksAQuestionNobodyNeededIsRejected(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		{Text: `{"intent":"clarification","title":"Groceries","priority":"normal",` +
			`"question":"Which shop do you use?","steps":[]}`},
		{Text: `{"intent":"task","title":"Buy Groceries","priority":"normal",` +
			`"steps":["Go to the shop","Buy milk"]}`},
	}}

	outcome, err := newService(t, generator, 1).
		Plan(context.Background(), testUserID, "buy groceries")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.Intent != domain.PlanIntentTask {
		t.Errorf("final intent = %q, want task: the pointless question should have been retried",
			outcome.Plan.Intent)
	}

	if outcome.Attempts < 2 {
		t.Error("a pointless clarification was accepted on the first attempt")
	}
}

// A model that ignores the table and reaches the calendar for an errand is the exact
// bug the table exists to stop.
func TestAModelThatIgnoresTheTableIsRejected(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		{Text: `{"intent":"calendar_event","title":"Groceries","priority":"normal",` +
			`"due_at":"2026-10-09T15:00:00Z","steps":[]}`},
		{Text: `{"intent":"task","title":"Buy Groceries","priority":"normal",` +
			`"steps":["Go to the shop","Buy milk"]}`},
	}}

	outcome, err := newService(t, generator, 1).
		Plan(context.Background(), testUserID, "buy groceries")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.Intent != domain.PlanIntentTask {
		t.Errorf("final intent = %q, want task", outcome.Plan.Intent)
	}

	if outcome.Attempts < 2 {
		t.Error("a calendar event for an errand was accepted on the first attempt")
	}

	// The complaint has to name the required intent, because it is the only place the
	// model is told what it got wrong.
	if len(outcome.ValidationErrors) == 0 ||
		!strings.Contains(outcome.ValidationErrors[0], "task") {
		t.Errorf("validation errors = %v, want one naming the required intent",
			outcome.ValidationErrors)
	}
}

// When the model cannot be made to produce a clarification, the fallback is the
// question rather than a task. A task titled "Dentist appointment" with no time is
// the thing that produced the original bug.
func TestAnUntimedAppointmentFallsBackToAQuestionNotATask(t *testing.T) {
	generator := &fakeGenerator{}

	outcome, err := newService(t, generator, 0).
		Plan(context.Background(), testUserID, "I need to see the dentist")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.Intent != domain.PlanIntentClarification {
		t.Fatalf("fallback intent = %q, want clarification", outcome.Plan.Intent)
	}

	if outcome.Plan.Question == "" {
		t.Error("the fallback clarification carries no question, so there is nothing to answer")
	}

	if outcome.Plan.DueAt != nil {
		t.Error("the fallback clarification carries a time it had to invent")
	}
}
