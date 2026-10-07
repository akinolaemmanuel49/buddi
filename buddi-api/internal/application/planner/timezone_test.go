package planner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// wednesdayNoon is the instant every date test runs against. Wednesday 7 October 2026,
// which is the day "next Friday" was reported wrong.
func wednesdayNoon() time.Time {
	return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
}

func london(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	return loc
}

// The reported bug: "next week" resolved to Friday the 9th instead of Friday the 16th.
//
// It was caused by the date table holding only seven days, so the 16th was not in it at
// all, and by checkWeekday demanding the 9th by name and rejecting anything else. The
// validator was not merely failing to catch the error, it was producing it.
func TestNextWeekMeansTheSecondOccurrence(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		// The model gets it right: the 16th.
		ok(`{"intent":"calendar_event","title":"Dentist Appointment","priority":"normal",` +
			`"due_at":"2026-10-16T15:00:00Z","steps":[]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{
		Model: "m",
		Clock: wednesdayNoon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.PlanIn(context.Background(), testUserID,
		"dentist next Friday at 3pm", london(t))
	if err != nil {
		t.Fatalf("PlanIn: %v", err)
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: a correct date for \"next Friday\" was rejected (%v)",
			outcome.Attempts, outcome.ValidationErrors)
	}

	if outcome.Plan.DueAt == nil {
		t.Fatal("no due date")
	}

	if got := outcome.Plan.DueAt.UTC().Format("2006-01-02"); got != "2026-10-16" {
		t.Errorf("due_at = %s, want Friday the 16th", got)
	}
}

// "next week" without a weekday still means the following week, and a bare weekday means
// the soonest one. Conflating the two moved an appointment a whole week.
func TestNextWeekAndABareWeekdayDiffer(t *testing.T) {
	tests := []struct {
		goal string
		want string
	}{
		{"dentist on Friday at 3pm", "2026-10-09"},
		{"dentist next Friday at 3pm", "2026-10-16"},
		{"dentist this Friday at 3pm", "2026-10-09"},
		{"dentist on Friday next week at 3pm", "2026-10-16"},
	}

	for _, tt := range tests {
		generator := &fakeGenerator{responses: []planner.Response{
			ok(`{"intent":"calendar_event","title":"Dentist","priority":"normal",` +
				`"due_at":"` + tt.want + `T15:00:00Z","steps":[]}`),
		}}

		service, err := planner.NewService(generator, planner.Options{
			Model: "m",
			Clock: wednesdayNoon,
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}

		outcome, err := service.PlanIn(context.Background(), testUserID, tt.goal, london(t))
		if err != nil {
			t.Fatalf("PlanIn(%q): %v", tt.goal, err)
		}

		if outcome.Attempts != 1 {
			t.Errorf("PlanIn(%q) took %d attempts, want 1: the correct date was rejected (%v)",
				tt.goal, outcome.Attempts, outcome.ValidationErrors)
		}
	}
}

// The table has to contain both occurrences of each weekday, because a table holding
// only the first cannot express "next Friday" at all.
func TestTheDateTableOffersBothOccurrencesOfEveryWeekday(t *testing.T) {
	generator := &promptCapturing{}

	service, err := planner.NewService(generator, planner.Options{
		Model: "m",
		Clock: wednesdayNoon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := service.PlanIn(context.Background(), testUserID, "dentist", london(t)); err != nil {
		t.Fatalf("PlanIn: %v", err)
	}

	prompt := generator.prompt

	for _, want := range []string{
		// The 9th is this week's Friday, the 16th is next week's.
		"2026-10-09",
		"2026-10-16",
		"the second is what",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("date table is missing %q:\n%s", want, prompt)
		}
	}
}

// The reported bug: a 3pm appointment reached the calendar as 4pm.
//
// The plan's instant is unambiguous; what was missing was the zone that gives it
// meaning. 15:00Z is 16:00 in London while BST is in force, and Google renders an
// instant in the calendar's own zone.
func TestTheZoneThePlanWasMadeInIsRecordedOnIt(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"intent":"calendar_event","title":"Dentist","priority":"normal",` +
			`"due_at":"2026-10-09T15:00:00+01:00","steps":[]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{
		Model: "m",
		Clock: wednesdayNoon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.PlanIn(context.Background(), testUserID,
		"dentist on Friday at 3pm", london(t))
	if err != nil {
		t.Fatalf("PlanIn: %v", err)
	}

	if outcome.Plan.TimeZone != "Europe/London" {
		t.Errorf("TimeZone = %q, want Europe/London: without it the instant is ambiguous",
			outcome.Plan.TimeZone)
	}

	if got := outcome.Plan.DueAt.In(outcome.Plan.Zone()).Hour(); got != 15 {
		t.Errorf("the plan reads as %02d:00 in the user's zone, want 15:00", got)
	}

	// The stored instant is 14:00Z, which is 15:00 in London. Both facts have to hold.
	if got := outcome.Plan.DueAt.UTC().Format("15:04"); got != "14:00" {
		t.Errorf("stored instant = %s UTC, want 14:00 for a 15:00 London appointment", got)
	}
}

// An unknown zone falls back to UTC rather than failing. The name arrives from a browser,
// so a renamed zone must not stop the user doing anything.
func TestAnUnknownZoneFallsBackToUTC(t *testing.T) {
	plan := &planner.Plan{TimeZone: "Mars/Olympus_Mons"}

	if got := plan.Zone().String(); got != "UTC" {
		t.Errorf("Zone() = %q, want UTC", got)
	}

	empty := &planner.Plan{}

	if got := empty.Zone().String(); got != "UTC" {
		t.Errorf("Zone() = %q, want UTC for an absent zone", got)
	}
}

// The weekday check must compare in the user's zone. In UTC a Thursday-evening
// appointment belongs to Friday for anyone east of Greenwich, so checking in UTC
// rejects the right date and accepts the wrong one.
func TestTheWeekdayCheckRunsInTheUsersZone(t *testing.T) {
	// 23:30 on Thursday the 8th in London is already Friday the 9th in UTC. The request
	// asks for Friday.
	when := time.Date(2026, time.October, 8, 23, 30, 0, 0, london(t))

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"intent":"calendar_event","title":"Late","priority":"normal",` +
			`"due_at":"` + when.Format(time.RFC3339) + `","steps":[]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{
		Model: "m",
		Clock: wednesdayNoon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.PlanIn(context.Background(), testUserID,
		"late appointment on Friday", london(t))
	if err != nil {
		t.Fatalf("PlanIn: %v", err)
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: Friday in the user's zone was rejected as Thursday (%v)",
			outcome.Attempts, outcome.ValidationErrors)
	}
}

// A deadline still outranks the weekday check. "before Friday" is a bound, and this
// change must not have made Thursday plans fail.
func TestADeadlineIsStillAllowedOnAnEarlierDay(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"intent":"task","title":"Buy milk","priority":"normal",` +
			`"due_at":"2026-10-08T18:00:00+01:00","steps":["Go to the shop"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{
		Model: "m",
		Clock: wednesdayNoon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.PlanIn(context.Background(), testUserID,
		"buy milk before Friday", london(t))
	if err != nil {
		t.Fatalf("PlanIn: %v", err)
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: a deadline was rejected by the weekday check",
			outcome.Attempts)
	}
}

var _ = domain.PlanIntentTask
