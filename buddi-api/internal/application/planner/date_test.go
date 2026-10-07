package planner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// The date table exists because the model cannot do calendar arithmetic: asked on a
// Wednesday for a Friday appointment it answered with a Thursday eight days out.
func TestPromptOffersAReadableDateTable(t *testing.T) {
	// A Wednesday.
	generator := &promptCapturing{}
	service := newService(t, generator, 0).WithClock(func() time.Time {
		return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	})

	if _, err := service.Plan(context.Background(), testUserID, "book the dentist on Friday"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	prompt := generator.prompt

	for _, want := range []string{
		"Wednesday",
		"Friday:    2026-10-09",
		"Do not do the arithmetic yourself",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// The title was coming back as an instruction rather than the subject of the item.
func TestPromptSteersTitlesTowardsNounPhrases(t *testing.T) {
	generator := &promptCapturing{}

	service := newService(t, generator, 0).WithClock(func() time.Time {
		return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	})

	if _, err := service.Plan(context.Background(), testUserID, "book the dentist"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !strings.Contains(generator.prompt, `"Dentist Appointment", never "Book Dentist Appointment"`) {
		t.Errorf("prompt does not steer the title towards a noun phrase:\n%s", generator.prompt)
	}
}

// A calendar event dated to a different weekday than the request named is rejected, so
// the retry can correct it instead of the wrong day reaching the user's calendar.
func TestACalendarEventOnTheWrongWeekdayIsRejected(t *testing.T) {
	// Wednesday 7 October 2026. Friday of that week is the 9th.
	clock := func() time.Time { return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC) }

	generator := &fakeGenerator{responses: []planner.Response{
		{Text: `{"intent":"calendar_event","title":"Dentist","priority":"normal",` +
			`"due_at":"2026-10-15T15:00:00Z","steps":["go"]}`},
		{Text: `{"intent":"calendar_event","title":"Dentist","priority":"normal",` +
			`"due_at":"2026-10-09T15:00:00Z","steps":["go"]}`},
	}}

	service := newService(t, generator, 1).WithClock(clock)

	outcome, err := service.Plan(context.Background(), testUserID, "dentist appointment on Friday at 3pm")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: the wrong weekday must be rejected and retried",
			outcome.Attempts)
	}

	if outcome.UsedFallback {
		t.Error("UsedFallback = true, want the corrected plan")
	}

	if outcome.Plan == nil || outcome.Plan.DueAt == nil {
		t.Fatal("no plan")
	}

	if got := outcome.Plan.DueAt.UTC().Format("2006-01-02"); got != "2026-10-09" {
		t.Errorf("due_at = %s, want Friday the 9th", got)
	}

	// The complaint has to name the right date, because the retry prompt is the only
	// place the model is told what went wrong.
	if len(outcome.ValidationErrors) == 0 ||
		!strings.Contains(outcome.ValidationErrors[0], "2026-10-09") {
		t.Errorf("validation errors = %v, want one naming the correct date",
			outcome.ValidationErrors)
	}
}

// A deadline is allowed to land on an earlier day than the weekday it names, so the
// check must not fire on "before Friday".
func TestADeadlineOnAnEarlierDayIsAccepted(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC) }

	generator := &fakeGenerator{responses: []planner.Response{{
		Text: `{"intent":"task","title":"Buy milk","priority":"normal",` +
			`"due_at":"2026-10-08T18:00:00Z","steps":["go"]}`,
	}}}

	service := newService(t, generator, 1).WithClock(clock)

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk before Friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: a deadline need not fall on the named day",
			outcome.Attempts)
	}
}

// Two weekdays named means there is no single day to check against.
func TestTwoWeekdaysAreNotChecked(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC) }

	generator := &fakeGenerator{responses: []planner.Response{{
		Text: `{"intent":"calendar_event","title":"Sync","priority":"normal",` +
			`"due_at":"2026-10-08T15:00:00Z","steps":["go"]}`,
	}}}

	service := newService(t, generator, 1).WithClock(clock)

	outcome, err := service.Plan(context.Background(), testUserID, "sync between Monday and Friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: an ambiguous request must not be rejected",
			outcome.Attempts)
	}
}

var _ = domain.PlanIntentTask
