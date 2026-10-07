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

	// The wording matters. It used to say "use task for anything that needs tracking",
	// which reads as a default and a calendar request satisfies it, so the model chose
	// task. It has since become narrower in the other direction too: an errand with no
	// time in it is a task even when it sounds like something to schedule, which is why
	// "groceries" is named explicitly.
	if !strings.Contains(schema, "at a time") {
		t.Error("intent description does not make the time the condition for calendar_event")
	}

	if !strings.Contains(schema, "task for anything else") {
		t.Error("intent description still makes task sound like the catch-all")
	}

	if !strings.Contains(schema, "clarification") {
		t.Error("intent description does not offer clarification for missing information")
	}
}

// An errand with no time in it is a task, not an event. This was reported as "why did
// buying groceries put something on my calendar", and the schema is where the answer
// has to be given because the schema is what the model reads first.
func TestSchemaSendsUntimedErrandsToATask(t *testing.T) {
	schema := capturedSchema(t)

	if !strings.Contains(schema, "groceries") {
		t.Error("intent description does not route an untimed errand to a task")
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
//
// steps is deliberately absent from this list. It used to be required of every plan,
// which meant a calendar event had to invent an action to satisfy the schema — and
// because steps were the only free text the event description was built from, the
// invented action became the text in the user's calendar.
func TestSchemaRequiresAnIntentButNotSteps(t *testing.T) {
	var schema struct {
		Required  []string `json:"required"`
		StepsSpec struct {
			MinItems *int `json:"minItems"`
		} `json:"-"`
	}

	if err := json.Unmarshal([]byte(capturedSchema(t)), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	for _, want := range []string{"intent", "title", "priority"} {
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

	for _, field := range schema.Required {
		if field == "steps" {
			t.Error("steps is required: an appointment would have to invent an action to satisfy it")
		}
	}
}

// minItems is what actually forces a model to invent content, so it is asserted
// directly rather than inferred from the wording of a description.
func TestSchemaDoesNotForceAtLeastOneStep(t *testing.T) {
	var schema struct {
		Properties struct {
			Steps struct {
				MinItems *int `json:"minItems"`
			} `json:"steps"`
		} `json:"properties"`
	}

	if err := json.Unmarshal([]byte(capturedSchema(t)), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	if schema.Properties.Steps.MinItems != nil {
		t.Errorf("steps has minItems %d, want none", *schema.Properties.Steps.MinItems)
	}
}

// A clarification is only useful if the planner puts a question in it.
func TestSchemaCarriesAQuestionForClarifications(t *testing.T) {
	schema := capturedSchema(t)

	if !strings.Contains(schema, "question") {
		t.Error("schema has no question property")
	}

	if !strings.Contains(schema, "clarification") {
		t.Error("schema does not mention clarification")
	}
}

// The event detail needs somewhere to go. "Dentist at 3pm with Dr Y" is three pieces
// of information and only one of them was ever carried.
func TestSchemaCarriesLocationAndAttendees(t *testing.T) {
	schema := capturedSchema(t)

	for _, field := range []string{"location", "attendees"} {
		if !strings.Contains(schema, `"`+field+`"`) {
			t.Errorf("schema has no %s property", field)
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

	if !strings.Contains(prompt, "- intent: must be calendar_event") {
		t.Errorf("prompt does not state the routed intent for an appointment:\n%s", prompt)
	}
}

// The routing table decides the intent, so the prompt's job for this field is to state
// the verdict rather than the rules. The rules themselves are tested in routing_test.go
// and reach the model through the schema.
func TestPromptStatesTheRoutedIntentRatherThanTheRules(t *testing.T) {
	generator := &capturingGenerator{}
	promptHolder := &promptCapturing{inner: generator}

	if _, err := newService(t, promptHolder, 0).
		Plan(context.Background(), testUserID, "buy groceries"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	prompt := promptHolder.prompt

	if !strings.Contains(prompt, "- intent: must be task") {
		t.Errorf("prompt does not route an errand to a task:\n%s", prompt)
	}
}

// An appointment with no time in it has to become a question rather than an event with
// an invented start, so the prompt has to say that in as many words.
func TestPromptTurnsAnUntimedAppointmentIntoAQuestion(t *testing.T) {
	generator := &capturingGenerator{}
	promptHolder := &promptCapturing{inner: generator}

	if _, err := newService(t, promptHolder, 0).
		Plan(context.Background(), testUserID, "I need to see the dentist"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	prompt := promptHolder.prompt

	if !strings.Contains(prompt, "- intent: must be clarification") {
		t.Errorf("prompt does not ask for the missing time:\n%s", prompt)
	}

	if !strings.Contains(prompt, "invent no time") {
		t.Errorf("prompt does not forbid inventing the time:\n%s", prompt)
	}
}

func TestPromptRepeatsTheRoutingRules(t *testing.T) {
	generator := &capturingGenerator{}
	promptHolder := &promptCapturing{inner: generator}

	if _, err := newService(t, promptHolder, 0).
		Plan(context.Background(), testUserID, "buy groceries"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	prompt := promptHolder.prompt

	for _, want := range []string{
		"clarification",
		"Leave empty for a calendar_event",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
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
