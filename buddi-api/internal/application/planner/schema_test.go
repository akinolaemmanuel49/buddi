package planner_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
)

// capturingGenerator records the schema it was asked for and returns a usable plan.
type capturingGenerator struct {
	schema string
}

func (g *capturingGenerator) Generate(_ context.Context, req planner.Request) (planner.Response, error) {
	g.schema = string(req.Schema)

	return planner.Response{
		Text: `{"intent":"task","title":"x","priority":"normal","steps":["y"]}`,
	}, nil
}

// capturedSchema runs one plan and returns the schema that reached the model.
func capturedSchema(t *testing.T) string {
	t.Helper()

	generator := &capturingGenerator{}

	if _, err := newService(t, generator, 0).Plan(context.Background(), testUserID, "do a thing"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.schema == "" {
		t.Fatal("no schema reached the generator")
	}

	return generator.schema
}

// A calendar request produced a plan with intent "task" and no due_at, so the registry
// created a task and no event was ever proposed. Both are schema-level mistakes rather
// than model mistakes, which is why they are pinned here.
func TestSchemaGuidesCalendarIntents(t *testing.T) {
	schema := capturedSchema(t)

	if !strings.Contains(schema, "calendar_event") {
		t.Fatal("schema does not mention calendar_event")
	}

	// The wording matters: "use task for anything that needs tracking" reads as a
	// default and a calendar request satisfies it, so the model chose task.
	if !strings.Contains(schema, "whenever the user") {
		t.Error("intent description does not point at calendar_event for calendar requests")
	}

	if !strings.Contains(schema, "task for anything else") {
		t.Error("intent description still makes task sound like the catch-all")
	}
}

// An event's start time is not a deadline, so a due_at described only as a deadline
// left nowhere to put "Friday at 3pm" and it was dropped.
func TestSchemaTreatsDueAtAsAnEventStartTime(t *testing.T) {
	schema := capturedSchema(t)

	if !strings.Contains(schema, "starts") {
		t.Error("due_at description does not say it is the start time for a calendar event")
	}
}

// An omitted intent was silently defaulted to task, which is how a calendar request
// became a task. Requiring it forces a deliberate choice.
func TestSchemaRequiresAnIntent(t *testing.T) {
	var schema struct {
		Required []string `json:"required"`
	}

	if err := json.Unmarshal([]byte(capturedSchema(t)), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	for _, want := range []string{"intent", "title", "priority", "steps"} {
		found := false

		for _, field := range schema.Required {
			if field == want {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("required is missing %q, got %v", want, schema.Required)
		}
	}
}

// The prompt repeats the same rules as the schema. When the two disagree the model
// follows the prompt, so the prompt has to carry the calendar case too.
func TestPromptStatesThatAnEventTimeGoesInDueAt(t *testing.T) {
	generator := &capturingGenerator{}
	promptHolder := &promptCapturing{inner: generator}

	if _, err := newService(t, promptHolder, 0).
		Plan(context.Background(), testUserID, "add a dentist appointment on Friday at 3pm"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	prompt := promptHolder.prompt

	if !strings.Contains(prompt, "start time for a calendar_event") {
		t.Errorf("prompt does not say an event's time belongs in due_at:\n%s", prompt)
	}

	if !strings.Contains(prompt, "calendar_event when the request wants something put on a calendar") {
		t.Errorf("prompt does not tell the model when to choose calendar_event:\n%s", prompt)
	}
}

// promptCapturing records the prompt alongside the schema.
type promptCapturing struct {
	inner  *capturingGenerator
	prompt string
}

func (g *promptCapturing) Generate(_ context.Context, req planner.Request) (planner.Response, error) {
	g.prompt = req.Prompt

	if g.inner != nil {
		g.inner.schema = string(req.Schema)
	}

	return planner.Response{
		Text: `{"intent":"calendar_event","title":"x","priority":"normal","steps":["y"]}`,
	}, nil
}
