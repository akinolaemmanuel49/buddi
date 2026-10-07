package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// testUserID is the owner every plan below is made for. Retrieval is tenant scoped,
// so a plan for the zero uuid is a plan with no groundable notes.
var testUserID = uuid.New()

// fakeGenerator returns queued responses and records the prompts it was given.
type fakeGenerator struct {
	responses []planner.Response
	errs      []error

	calls   int
	prompts []string
	schemas []string
}

func (f *fakeGenerator) Generate(_ context.Context, req planner.Request) (planner.Response, error) {
	f.calls++
	f.prompts = append(f.prompts, req.Prompt)
	f.schemas = append(f.schemas, string(req.Schema))

	i := f.calls - 1

	var (
		res planner.Response
		err error
	)

	if i < len(f.errs) && f.errs[i] != nil {
		err = f.errs[i]
	}

	if i < len(f.responses) {
		res = f.responses[i]
	}

	return res, err
}

func newService(t *testing.T, generator planner.Generator, retries int) *planner.Service {
	t.Helper()

	service, err := planner.NewService(generator, planner.Options{
		Model:      "test-model",
		MaxRetries: retries,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

func ok(text string) planner.Response {
	return planner.Response{Text: text, Model: "test-model", Elapsed: time.Second}
}

// The schema is derived from the domain, so it must offer exactly the
// priorities a Task accepts. It once offered "medium", which no task could ever
// have, so every plan was rejected by the very entity it was meant to become.
func TestSchemaOffersExactlyTheDomainPriorities(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"normal","steps":["Go to the shop"]}`),
	}}

	if _, err := newService(t, generator, 0).Plan(context.Background(), testUserID, "buy milk"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 1 {
		t.Fatalf("calls = %d, want 1", generator.calls)
	}

	var schema struct {
		Properties struct {
			Priority struct {
				Enum []string `json:"enum"`
			} `json:"priority"`
		} `json:"properties"`
	}

	if err := json.Unmarshal([]byte(generator.schemas[0]), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	want := make([]string, 0, len(domain.AllTaskPriorities))
	for _, p := range domain.AllTaskPriorities {
		want = append(want, string(p))
	}

	if len(schema.Properties.Priority.Enum) != len(want) {
		t.Fatalf("priority enum = %v, want %v", schema.Properties.Priority.Enum, want)
	}

	for i := range want {
		if schema.Properties.Priority.Enum[i] != want[i] {
			t.Errorf("priority enum = %v, want %v", schema.Properties.Priority.Enum, want)
		}
	}
}

func TestPlanAcceptsAValidResponse(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy semi-skimmed milk","description":"Before Friday","priority":"high","due_at":"2026-11-01T09:00:00Z","steps":["Check the fridge","Go to the shop","Buy two bottles"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "  buy semi skimmed milk before friday  ")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.UsedFallback {
		t.Error("a valid plan was replaced by the fallback")
	}

	plan := outcome.Plan

	if plan.Title != "Buy semi-skimmed milk" {
		t.Errorf("Title = %q", plan.Title)
	}

	if plan.Priority != domain.TaskPriorityHigh {
		t.Errorf("Priority = %q, want high", plan.Priority)
	}

	if plan.DueAt == nil || plan.DueAt.Year() != 2026 {
		t.Errorf("DueAt = %v", plan.DueAt)
	}

	if len(plan.Steps) != 3 {
		t.Errorf("Steps = %d, want 3", len(plan.Steps))
	}

	if outcome.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", outcome.Attempts)
	}
}

// The smoke run produced run-on titles that restate the whole request. Those are
// legal task titles, so nothing downstream would catch them.
func TestPlanRejectsRunOnTitlesAndRetries(t *testing.T) {
	runOn := `{"title":"Plan to purchase semi-skimmed milk before Friday, with the following details: 1. Urgency: Immediate action needed. 2. Note: Shop closes at 6:00 PM.","priority":"high","steps":["Buy it"]}`

	generator := &fakeGenerator{responses: []planner.Response{
		ok(runOn),
		ok(`{"title":"Buy semi-skimmed milk","priority":"high","steps":["Go to the shop"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk before friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 2 {
		t.Errorf("calls = %d, want 2 (one rejection then one retry)", generator.calls)
	}

	if len(outcome.Plan.Title) > 120 {
		t.Errorf("Title is still %d characters: %q", len(outcome.Plan.Title), outcome.Plan.Title)
	}

	if outcome.UsedFallback {
		t.Error("the retry succeeded, so the fallback should not have been used")
	}
}

// The retry has to say what was wrong, or it is just the same request again.
func TestRetryPromptCarriesTheValidationErrors(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"","steps":[]}`),
		ok(`{"title":"Buy milk","priority":"normal","steps":["Go to the shop"]}`),
	}}

	if _, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(generator.prompts) != 2 {
		t.Fatalf("prompts recorded = %d, want 2", len(generator.prompts))
	}

	retry := generator.prompts[1]

	for _, want := range []string{"title must not be empty", "steps"} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry prompt does not mention %q:\n%s", want, retry)
		}
	}
}

// A malformed optional field must not cost a round trip. It is repaired and
// noted instead, because retrying for a bad due date discarded an otherwise good
// plan on every attempt in a live run.
func TestPlanDropsAnUnparseableDueDateInsteadOfFailing(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"high","due_at":"next Friday sometime","steps":["Go to the shop"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk before friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 1 {
		t.Errorf("calls = %d, want 1 without a retry", generator.calls)
	}

	if outcome.UsedFallback {
		t.Fatal("a bad optional field should not discard the plan")
	}

	if outcome.Plan.Title != "Buy milk" || outcome.Plan.Priority != domain.TaskPriorityHigh {
		t.Errorf("plan = %+v, want the usable parts kept", outcome.Plan)
	}

	if outcome.Plan.DueAt != nil {
		t.Errorf("DueAt = %v, want nil", outcome.Plan.DueAt)
	}

	if len(outcome.Notes) == 0 || !strings.Contains(outcome.Notes[0], "due_at") {
		t.Errorf("Notes = %v, want the dropped due date recorded", outcome.Notes)
	}
}

// The model has no clock, so without the date in the prompt it invents one. A
// live run answered "before Friday" with a due date three years in the past.
func TestPlanDropsADueDateInThePast(t *testing.T) {
	now := time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC)

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"high","due_at":"2023-10-25T18:00:00Z","steps":["Go to the shop"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{Model: "test-model", MaxRetries: 1, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk before friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.DueAt != nil {
		t.Errorf("DueAt = %v, want a stale date dropped", outcome.Plan.DueAt)
	}

	if outcome.UsedFallback {
		t.Error("the plan was usable once the stale date was dropped")
	}
}

func TestPlanKeepsADueDateInTheFuture(t *testing.T) {
	now := time.Date(2026, time.October, 4, 9, 0, 0, 0, time.UTC)

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"high","due_at":"2026-10-09T18:00:00Z","steps":["Go to the shop"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{Model: "test-model", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk before friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	wantDue := time.Date(2026, time.October, 9, 18, 0, 0, 0, time.UTC)

	if outcome.Plan.DueAt == nil || !outcome.Plan.DueAt.Equal(wantDue) {
		t.Errorf("DueAt = %v, want %v", outcome.Plan.DueAt, wantDue)
	}

	if len(outcome.Notes) != 0 {
		t.Errorf("Notes = %v, want none", outcome.Notes)
	}
}

// Telling the model the current time stopped it inventing dates years in the
// past, but a 0.6b model does not do date arithmetic: it answered "before Friday"
// with the instant it had just been given. That is not a usable deadline.
func TestPlanDropsADueDateThatEchoesTheRequestTime(t *testing.T) {
	now := time.Date(2026, time.October, 4, 7, 30, 0, 0, time.UTC)

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"high","due_at":"2026-10-04T07:29:40Z","steps":["Go to the shop"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{Model: "m", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk before friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.DueAt != nil {
		t.Errorf("DueAt = %v, want the echoed request time dropped", outcome.Plan.DueAt)
	}

	if outcome.UsedFallback {
		t.Error("the rest of the plan was usable")
	}

	if len(outcome.Notes) == 0 || !strings.Contains(outcome.Notes[0], "artefact") {
		t.Errorf("Notes = %v, want the artefact recorded", outcome.Notes)
	}
}

// A real deadline only a few hours out must survive the artefact check.
func TestPlanKeepsADueDateSoonAfterNow(t *testing.T) {
	now := time.Date(2026, time.October, 4, 7, 30, 0, 0, time.UTC)

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"high","due_at":"2026-10-04T18:00:00Z","steps":["Go to the shop"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{Model: "m", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	outcome, err := service.Plan(context.Background(), testUserID, "buy milk before 6pm")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.DueAt == nil {
		t.Error("a deadline ten hours out was dropped as an artefact")
	}
}

// Without the current date the model cannot resolve "Friday", which is the root
// cause of the invented past dates rather than a symptom to filter afterwards.
func TestPromptIncludesTheCurrentDate(t *testing.T) {
	now := time.Date(2026, time.October, 4, 9, 30, 0, 0, time.UTC)

	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"normal","steps":["Go"]}`),
	}}

	service, err := planner.NewService(generator, planner.Options{Model: "m", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := service.Plan(context.Background(), testUserID, "buy milk before friday"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !strings.Contains(generator.prompts[0], "2026-10-04T09:30:00Z") {
		t.Errorf("prompt does not carry the current date:\n%s", generator.prompts[0])
	}
}

// An absent priority is a reasonable gap, not an error worth burning a retry on.
func TestPlanDefaultsAnAbsentPriority(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","steps":["Go to the shop"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.Plan.Priority != domain.TaskPriorityNormal {
		t.Errorf("Priority = %q, want normal", outcome.Plan.Priority)
	}

	if generator.calls != 1 {
		t.Errorf("calls = %d, want 1 without a retry", generator.calls)
	}
}

// An invalid priority is defaulted with a note rather than rejected, since it is
// repairable and no task could hold the value anyway.
func TestPlanDefaultsAnUnusablePriority(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"medium","steps":["Go to the shop"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 1 {
		t.Errorf("calls = %d, want 1 without a retry", generator.calls)
	}

	if outcome.UsedFallback {
		t.Error("a bad priority should not discard the plan")
	}

	if outcome.Plan.Priority != domain.TaskPriorityNormal {
		t.Errorf("Priority = %q, want normal", outcome.Plan.Priority)
	}

	if !outcome.Plan.Priority.IsValid() {
		t.Error("the defaulted priority cannot be stored on a task")
	}

	if len(outcome.Notes) == 0 || !strings.Contains(outcome.Notes[0], "priority") {
		t.Errorf("Notes = %v, want the defaulted priority recorded", outcome.Notes)
	}
}

func TestPlanFallsBackWhenTheModelKeepsFailing(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok("not json at all"),
		ok(`{"title":"","steps":[]}`),
		ok(`{}`),
	}}

	outcome, err := newService(t, generator, 2).Plan(context.Background(), testUserID, "Buy semi-skimmed milk before Friday")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 3 {
		t.Errorf("calls = %d, want 3 (initial plus two retries)", generator.calls)
	}

	if !outcome.UsedFallback {
		t.Fatal("UsedFallback = false, want the deterministic plan")
	}

	plan := outcome.Plan

	if plan.Title != "Buy semi-skimmed milk before Friday" {
		t.Errorf("fallback Title = %q, want the goal itself", plan.Title)
	}

	if plan.Priority != domain.TaskPriorityNormal {
		t.Errorf("fallback Priority = %q, want normal", plan.Priority)
	}

	if len(plan.Steps) != 1 || plan.Steps[0].Description != "Buy semi-skimmed milk before Friday" {
		t.Errorf("fallback Steps = %+v, want the goal as a single step", plan.Steps)
	}

	if len(outcome.ValidationErrors) == 0 {
		t.Error("ValidationErrors is empty, so the failure would be undiagnosable")
	}
}

// A transport error will not be fixed by a better prompt, so it must not be
// retried. On a CPU runtime that is the difference between one failed call and
// three.
func TestPlanDoesNotRetryAGenerationError(t *testing.T) {
	generator := &fakeGenerator{
		responses: []planner.Response{{}},
		errs:      []error{errors.New("connection refused")},
	}

	outcome, err := newService(t, generator, 2).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if generator.calls != 1 {
		t.Errorf("calls = %d, want 1", generator.calls)
	}

	if !outcome.UsedFallback {
		t.Error("UsedFallback = false, want the deterministic plan")
	}
}

// A timeout is not the model answering badly, it is the model never answering.
// Falling back would return the user's own goal as a plan that reads exactly like
// a considered one, and they would approve it believing the model had thought
// about the request. Found live: a 2s planning budget produced awaiting_approval
// with a fallback plan, no error and no fallback marker.
func TestPlanReturnsADeadlineErrorInsteadOfFallingBack(t *testing.T) {
	generator := &fakeGenerator{errs: []error{context.DeadlineExceeded}}

	outcome, err := newService(t, generator, 2).Plan(context.Background(), testUserID, "buy milk")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	if outcome != nil {
		t.Errorf("outcome = %+v, want nil so no plan can be mistaken for one", outcome)
	}

	if generator.calls != 1 {
		t.Errorf("calls = %d, want 1: a cancelled context cannot be retried", generator.calls)
	}
}

func TestPlanReturnsACancellationInsteadOfFallingBack(t *testing.T) {
	generator := &fakeGenerator{errs: []error{context.Canceled}}

	if _, err := newService(t, generator, 2).Plan(context.Background(), testUserID, "buy milk"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The error has to survive wrapping, because the timeout arrives from the HTTP
// client inside a url.Error rather than as a bare context error.
func TestPlanSeesAWrappedDeadlineError(t *testing.T) {
	generator := &fakeGenerator{errs: []error{
		fmt.Errorf("ollama: POST /api/generate: %w", context.DeadlineExceeded),
	}}

	if _, err := newService(t, generator, 2).Plan(context.Background(), testUserID, "buy milk"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wrapped deadline to be recognised", err)
	}
}

// A thinking model can spend its whole budget reasoning and return nothing. That
// is a budgeting failure, and it must not be reported as a model failure.
func TestPlanTreatsAnEmptyResponseAsInvalid(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(""),
		ok(`{"title":"Buy milk","priority":"normal","steps":["Go to the shop"]}`),
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.UsedFallback {
		t.Error("the retry succeeded, so the fallback should not have been used")
	}

	if len(outcome.ValidationErrors) == 0 || !strings.Contains(outcome.ValidationErrors[0], "empty") {
		t.Errorf("ValidationErrors = %v, want the empty response called out", outcome.ValidationErrors)
	}
}

func TestPlanAcceptsAFencedResponse(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok("```json\n{\"title\":\"Buy milk\",\"priority\":\"normal\",\"steps\":[\"Go to the shop\"]}\n```"),
	}}

	outcome, err := newService(t, generator, 0).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.UsedFallback || outcome.Plan.Title != "Buy milk" {
		t.Errorf("plan = %+v, fallback = %v", outcome.Plan, outcome.UsedFallback)
	}
}

func TestPlanNormalisesAndBoundsSteps(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		ok(`{"title":"Buy milk","priority":"normal","steps":["  Go to the shop  ","","   ","Buy two bottles"]}`),
	}}

	outcome, err := newService(t, generator, 0).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	steps := outcome.Plan.Steps
	if len(steps) != 2 {
		t.Fatalf("Steps = %+v, want blanks dropped", steps)
	}

	if steps[0].Description != "Go to the shop" {
		t.Errorf("Steps[0] = %q, want it trimmed", steps[0].Description)
	}
}

func TestPlanRejectsABlankGoal(t *testing.T) {
	generator := &fakeGenerator{}

	_, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "   ")
	if err == nil {
		t.Fatal("Plan accepted a blank goal")
	}

	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}

	if generator.calls != 0 {
		t.Errorf("calls = %d, want 0 for a goal that never reaches the model", generator.calls)
	}
}

func TestPlanRecordsCostAndAttempts(t *testing.T) {
	generator := &fakeGenerator{responses: []planner.Response{
		{Text: `{"title":"","steps":[]}`, ThinkingTokens: 180, Elapsed: 2 * time.Second, Model: "m"},
		{Text: `{"title":"Buy milk","priority":"low","steps":["Go"]}`, ThinkingTokens: 12, Elapsed: time.Second, Model: "m"},
	}}

	outcome, err := newService(t, generator, 1).Plan(context.Background(), testUserID, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if outcome.TotalThinkTokens != 192 {
		t.Errorf("TotalThinkTokens = %d, want 192 across both attempts", outcome.TotalThinkTokens)
	}

	if outcome.TotalElapsed != 3*time.Second {
		t.Errorf("TotalElapsed = %v, want 3s across both attempts", outcome.TotalElapsed)
	}

	if outcome.Model != "m" {
		t.Errorf("Model = %q, want m", outcome.Model)
	}

	if outcome.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", outcome.Attempts)
	}
}

// The fallback title is the user's own words, so an over-long goal must not be
// left as an invalid task or cut mid-word.
func TestFallbackTitleIsBoundedAndWhole(t *testing.T) {
	long := strings.Repeat("alpha beta ", 40)

	generator := &fakeGenerator{responses: []planner.Response{ok("garbage")}}

	outcome, err := newService(t, generator, 0).Plan(context.Background(), testUserID, long)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	title := outcome.Plan.Title

	if len(title) > 120 {
		t.Errorf("title is %d characters, want at most 120", len(title))
	}

	if !strings.HasSuffix(title, "alpha") && !strings.HasSuffix(title, "beta") {
		t.Errorf("title %q appears to end mid-word", title)
	}

	if len(title) > domain.MaxTaskTitle {
		t.Errorf("fallback title would not pass task validation: %d characters", len(title))
	}
}

func TestNewServiceValidatesOptions(t *testing.T) {
	if _, err := planner.NewService(nil, planner.Options{}); err == nil {
		t.Error("NewService accepted a nil generator")
	}

	if _, err := planner.NewService(&fakeGenerator{}, planner.Options{MaxRetries: -1}); err == nil {
		t.Error("NewService accepted negative retries")
	}
}
