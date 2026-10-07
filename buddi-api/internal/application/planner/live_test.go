package planner_test

// Live planner checks, gated behind BUDDI_SMOKE_OLLAMA=1.
//
// These exist because the unit tests cannot reach the failure that matters. They drive a
// fake generator, so they prove the validator accepts a correct date and rejects a wrong
// one. They say nothing about whether the model picks the right date out of the table it
// is shown, which is the part that actually broke: "next Friday" came back as the 9th,
// and a 3pm appointment came back as 15:00Z and reached the calendar as 4pm.
//
// Both of those were found here rather than in a unit test, so this stays rather than
// being deleted after use.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
)

const livePlannerEnv = "BUDDI_SMOKE_OLLAMA"

// liveReferenceNow is a Wednesday, which is the day both reported date bugs were seen
// on: "on Friday" and "next Friday" resolve to different dates from a Wednesday and agree
// from most others, so most weekdays would hide both.
var liveReferenceNow = time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)

func livePlannerService(t *testing.T) *planner.Service {
	t.Helper()

	if os.Getenv(livePlannerEnv) != "1" {
		t.Skipf("set %s=1 to run against a live Ollama runtime", livePlannerEnv)
	}

	base := os.Getenv("BUDDI_OLLAMA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}

	model := os.Getenv("BUDDI_MODEL")
	if model == "" {
		model = "qwen3:4b-instruct-2507-q4_K_M"
	}

	client, err := ollama.New(base, ollama.Options{
		Model:         model,
		ContextLength: 16384,
		Timeout:       10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}

	service, err := planner.NewService(ollama.NewAdapter(client), planner.Options{
		Model: model,
		Clock: func() time.Time { return liveReferenceNow },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// TestLiveResolvesRelativeDatesToTheUsersOwnClock is the regression for both reported
// bugs at once: the right day, and the right hour in the user's own zone.
func TestLiveResolvesRelativeDatesToTheUsersOwnClock(t *testing.T) {
	service := livePlannerService(t)

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	cases := []struct {
		goal string
		want string
		note string
	}{
		{"dentist appointment on Friday at 3pm", "2026-10-09 15:00", "a plain weekday is the soonest one"},
		{"dentist appointment next Friday at 3pm", "2026-10-16 15:00", `"next" moves it a week`},
		{"dentist appointment this Friday at 3pm", "2026-10-09 15:00", `"this" does not`},
		{"dentist next week on Friday at 3pm", "2026-10-16 15:00", `"next week" also moves it a week`},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	for _, tc := range cases {
		t.Run(tc.goal, func(t *testing.T) {
			outcome, err := service.PlanIn(ctx, uuid.New(), tc.goal, loc)
			if err != nil {
				t.Fatalf("PlanIn: %v", err)
			}

			if outcome.Plan == nil {
				t.Fatalf("no plan; validation errors: %v", outcome.ValidationErrors)
			}

			t.Logf("intent=%s title=%q attempts=%d fallback=%v",
				outcome.Plan.Intent, outcome.Plan.Title, outcome.Attempts, outcome.UsedFallback)

			if outcome.Plan.DueAt != nil {
				t.Logf("stored %s, which reads as %s in the user's zone",
					outcome.Plan.DueAt.Format(time.RFC3339),
					outcome.Plan.DueAt.In(loc).Format("2006-01-02 15:04 MST"))
			}

			for _, note := range outcome.Notes {
				t.Logf("note: %s", note)
			}

			if outcome.Plan.Intent != "calendar_event" {
				t.Fatalf("intent = %s, want calendar_event (%s)",
					outcome.Plan.Intent, tc.note)
			}

			if outcome.Plan.DueAt == nil {
				t.Fatalf("no due_at (%s)", tc.note)
			}

			if got := outcome.Plan.DueAt.In(loc).Format("2006-01-02 15:04"); got != tc.want {
				t.Errorf("got %s, want %s in the user's own zone (%s)", got, tc.want, tc.note)
			}
		})
	}
}

// TestLiveRoutesToTheRightToolOrAsks is the live check on the routing table and the
// clarification path together, since both are decisions about intent and the model gets
// the last word on neither when the table matched.
func TestLiveRoutesToTheRightToolOrAsks(t *testing.T) {
	service := livePlannerService(t)

	cases := []struct {
		goal string
		want string
		note string
	}{
		{"buy groceries", "task", "an errand has no place on a calendar"},
		{"do the laundry", "task", "same"},
		{"I need to see the dentist", "clarification", "an appointment with no time is a question"},
		{"coffee with Ana on Tuesday at 2pm", "calendar_event", "a timed meeting is an event"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	for _, tc := range cases {
		t.Run(tc.goal, func(t *testing.T) {
			outcome, err := service.PlanIn(ctx, uuid.New(), tc.goal, time.UTC)
			if err != nil {
				t.Fatalf("PlanIn: %v", err)
			}

			if outcome.Plan == nil {
				t.Fatalf("no plan; validation errors: %v", outcome.ValidationErrors)
			}

			t.Logf("intent=%s title=%q question=%q steps=%d",
				outcome.Plan.Intent, outcome.Plan.Title, outcome.Plan.Question, len(outcome.Plan.Steps))

			if string(outcome.Plan.Intent) != tc.want {
				t.Errorf("intent = %s, want %s (%s)", outcome.Plan.Intent, tc.want, tc.note)
			}

			// A question nobody can answer is worse than no question, so an empty one is
			// a failure rather than a cosmetic problem.
			if tc.want == "clarification" && outcome.Plan.Question == "" {
				t.Error("the clarification carries no question, so there is nothing to answer")
			}

			// Steps were removed as the event description, and an appointment inventing
			// an action to satisfy the schema is how that came about.
			if outcome.Plan.Intent == "calendar_event" && len(outcome.Plan.Steps) > 0 {
				t.Errorf("a calendar event invented %d steps: %v", len(outcome.Plan.Steps), outcome.Plan.Steps)
			}
		})
	}
}
