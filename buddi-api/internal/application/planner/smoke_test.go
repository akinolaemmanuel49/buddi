package planner_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/llm/ollama"
)

// This exercises the planner against the real runtime rather than a fake, which
// is the only way to find out whether the model honours the schema and the
// prompt together. It is skipped unless BUDDI_SMOKE_PLANNER=1:
//
//	BUDDI_SMOKE_PLANNER=1 go test ./internal/application/planner/ -run Smoke -v
//
// Like the LLM smoke harness it reports rather than asserts thresholds, except
// for the one property that must hold: a plan that reaches the caller has to be
// usable as a task, or the whole feature is broken.
const plannerSmokeEnv = "BUDDI_SMOKE_PLANNER"

// timestampInTitle matches the ISO timestamps the model copies out of the prompt
// when it is asked to state the current time.
var timestampInTitle = regexp.MustCompile(`\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2})?)?`)

func livePlanner(t *testing.T) *planner.Service {
	t.Helper()

	if os.Getenv(plannerSmokeEnv) != "1" {
		t.Skipf("set %s=1 to run against a live Ollama runtime", plannerSmokeEnv)
	}

	base := os.Getenv("BUDDI_OLLAMA_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}

	client, err := ollama.New(base, ollama.Options{
		Model:         "qwen3:0.6b",
		ContextLength: 8192,
		Temperature:   0.2,
		KeepAlive:     2 * time.Minute,
		Timeout:       10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	service, err := planner.NewService(
		ollama.NewAdapter(client).WithThinkLevel("low"),
		planner.Options{Model: "qwen3:0.6b", MaxRetries: 1},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

func TestSmokePlannerAgainstLiveModel(t *testing.T) {
	service := livePlanner(t)

	goals := []string{
		"buy semi-skimmed milk before Friday, it is urgent, and the shop closes at 6pm",
		"remind me to email the quarterly report on Monday morning",
		"plan the release of version 2.0 including tests, docs and a changelog",
	}

	ctx := context.Background()

	var fallbacks int

	// metaPhrases are how a small model leaks its instructions into the answer.
	// One of them became the task title in a live run and still satisfied every
	// structural check, so the smoke has to look at the words.
	metaPhrases := []string{"correct json", "json response", "json object", "here is", "response:", "assistant"}

	for _, goal := range goals {
		start := time.Now()

		outcome, err := service.Plan(ctx, testUserID, goal)
		if err != nil {
			t.Fatalf("Plan(%q): %v", goal, err)
		}

		plan := outcome.Plan

		t.Logf("goal: %.50q", goal)
		t.Logf("  -> %.2fs, %d attempt(s), fallback=%v, ~%d reasoning tokens",
			time.Since(start).Seconds(), outcome.Attempts, outcome.UsedFallback, outcome.TotalThinkTokens)
		t.Logf("  -> title=%q priority=%q steps=%d due=%v", plan.Title, plan.Priority, len(plan.Steps), plan.DueAt)

		if outcome.UsedFallback {
			fallbacks++

			t.Logf("  -> fallback used, rejected: %v", outcome.ValidationErrors)
		}

		if len(outcome.Notes) > 0 {
			t.Logf("  -> repaired: %v", outcome.Notes)
		}

		// Whatever produced it, a plan that reaches the caller has to be a
		// usable task. This is the property the whole feature rests on.
		if plan.Title == "" {
			t.Errorf("plan has no title: %+v", plan)
		}

		if len(plan.Steps) == 0 {
			t.Errorf("plan has no steps: %+v", plan)
		}

		if !plan.Priority.IsValid() {
			t.Errorf("priority %q cannot be stored on a task", plan.Priority)
		}

		lowerTitle := strings.ToLower(plan.Title)

		for _, phrase := range metaPhrases {
			if strings.Contains(lowerTitle, phrase) {
				t.Errorf("title %q leaked instruction wording (%q)", plan.Title, phrase)
			}
		}

		// The prompt has to state the current time, and the model likes to paste it
		// straight into the title. A live run produced "Buy semi-skimmed milk at
		// 2026-10-04T08:27:56Z", which then became a real task title.
		if timestampInTitle.MatchString(plan.Title) {
			t.Errorf("title %q contains a timestamp copied from the prompt", plan.Title)
		}

		for i, step := range plan.Steps {
			if timestampInTitle.MatchString(step.Description) {
				t.Logf("  -> note: step %d also carries a timestamp: %q", i, step.Description)
			}
		}

		// A due date the request never mentioned is worse than none at all, so a
		// far-future one is treated as a guess rather than accepted silently.
		if plan.DueAt != nil && plan.DueAt.After(time.Now().Add(365*24*time.Hour)) {
			t.Errorf("due date %v looks invented for goal %q", plan.DueAt, goal)
		}
	}

	if fallbacks == len(goals) {
		t.Errorf("every goal fell back, so the model produced nothing usable")
	}
}
