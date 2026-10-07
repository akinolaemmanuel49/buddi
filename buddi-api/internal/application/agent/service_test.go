package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel/trace"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

var fixedNow = time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)

// fakeStore is an in-memory RunStore that records the order of writes, so tests
// can assert that a run exists before it is planned.
type fakeStore struct {
	mu sync.Mutex

	runs      map[uuid.UUID]*domain.AgentRun
	approvals map[uuid.UUID]*domain.Approval

	calls []string

	// rejectUnknownUser simulates another user's row existing.
	foreignRuns map[uuid.UUID]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs:        map[uuid.UUID]*domain.AgentRun{},
		approvals:   map[uuid.UUID]*domain.Approval{},
		foreignRuns: map[uuid.UUID]bool{},
	}
}

func (s *fakeStore) record(name string) {
	s.calls = append(s.calls, name)
}

func (s *fakeStore) CreateRun(_ context.Context, run *domain.AgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("CreateRun")

	stored := *run
	s.runs[run.ID] = &stored

	return nil
}

func (s *fakeStore) UpdateRun(_ context.Context, run *domain.AgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("UpdateRun")

	stored := *run
	s.runs[run.ID] = &stored

	return nil
}

func (s *fakeStore) GetRun(_ context.Context, userID uuid.UUID, runID uuid.UUID) (*domain.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok || run.UserID != userID {
		return nil, errNotFound
	}

	stored := *run

	return &stored, nil
}

func (s *fakeStore) ListRuns(_ context.Context, userID uuid.UUID, filter domain.RunFilter, _ domain.Page) ([]domain.AgentRun, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]domain.AgentRun, 0)

	for _, run := range s.runs {
		if run.UserID != userID {
			continue
		}

		if filter.Status != "" && run.Status != filter.Status {
			continue
		}

		out = append(out, *run)
	}

	return out, int64(len(out)), nil
}

func (s *fakeStore) CreateApproval(_ context.Context, approval *domain.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("CreateApproval")

	stored := *approval
	s.approvals[approval.ID] = &stored

	return nil
}

func (s *fakeStore) UpdateApproval(_ context.Context, approval *domain.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("UpdateApproval")

	stored := *approval
	s.approvals[approval.ID] = &stored

	return nil
}

func (s *fakeStore) GetApproval(_ context.Context, userID uuid.UUID, approvalID uuid.UUID) (*domain.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	approval, ok := s.approvals[approvalID]
	if !ok || approval.UserID != userID {
		return nil, errNotFound
	}

	stored := *approval

	return &stored, nil
}

func (s *fakeStore) ListApprovals(_ context.Context, userID uuid.UUID, runID uuid.UUID) ([]domain.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]domain.Approval, 0)

	for _, approval := range s.approvals {
		if approval.UserID == userID && approval.RunID == runID {
			out = append(out, *approval)
		}
	}

	return out, nil
}

var errNotFound = errors.New("not found")

// fakePlanner returns a canned outcome and records the goal it was given.
type fakePlanner struct {
	outcome *planner.Outcome
	err     error

	// onPlan runs inside Plan, so a test can cancel the request context at the
	// moment the model is being consulted.
	onPlan func(ctx context.Context)

	goals []string
}

func (p *fakePlanner) Plan(ctx context.Context, _ uuid.UUID, goal string) (*planner.Outcome, error) {
	p.goals = append(p.goals, goal)

	if p.onPlan != nil {
		p.onPlan(ctx)
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return p.outcome, p.err
}

// fakeTool records executions and can be told to fail.
type fakeTool struct {
	name string

	// readOnly makes the tool report that it changes nothing, which is what stops
	// the registry from resolving a plan to it.
	readOnly bool

	validateErr error
	executeErr  error

	executions []string
	arguments  []json.RawMessage
	runIDs     []uuid.UUID
	userIDs    []uuid.UUID
}

func (t *fakeTool) Name() string        { return t.name }
func (t *fakeTool) Description() string { return "test tool" }

// Mutating is true unless a test says otherwise. Approvals exist to gate changes,
// so a fake that defaults to read-only would make most of these tests propose
// nothing.
func (t *fakeTool) Mutating() bool { return !t.readOnly }

func (t *fakeTool) Validate(arguments json.RawMessage) error {
	if t.validateErr != nil {
		return t.validateErr
	}

	if len(arguments) == 0 || !json.Valid(arguments) {
		return errors.New("arguments are not valid JSON")
	}

	return nil
}

func (t *fakeTool) Execute(_ context.Context, userID uuid.UUID, runID uuid.UUID, arguments json.RawMessage) (json.RawMessage, error) {
	t.executions = append(t.executions, string(arguments))
	t.runIDs = append(t.runIDs, runID)
	t.userIDs = append(t.userIDs, userID)

	if t.executeErr != nil {
		return nil, t.executeErr
	}

	return json.RawMessage(`{"task_id":"` + uuid.NewString() + `"}`), nil
}

// onlyRun returns the single stored run, failing the test if there is not
// exactly one.
func (s *fakeStore) onlyRun(t *testing.T) *domain.AgentRun {
	t.Helper()

	if len(s.runs) != 1 {
		t.Fatalf("runs stored = %d, want 1", len(s.runs))
	}

	for _, run := range s.runs {
		return run
	}

	return nil
}

func goodPlan() *planner.Outcome {
	return &planner.Outcome{
		Plan: &planner.Plan{
			Title:       "Buy semi-skimmed milk",
			Description: "Before Friday",
			Priority:    domain.TaskPriorityHigh,
			Steps:       []planner.Step{{Description: "Check the fridge"}, {Description: "Go to the shop"}},
		},
		Model: "test-model",
	}
}

// newService builds a service over a registry of the given tools.
//
// Each tool is bound to the task intent and the first is the default, which is all
// these tests need: they exercise one tool at a time, and the mapping between an
// intent and a mechanism has its own tests.
func newService(t *testing.T, store *fakeStore, p *fakePlanner, tools ...agent.Tool) *agent.Service {
	t.Helper()

	options := make([]agent.RegistryOption, 0, len(tools))
	for i, tool := range tools {
		options = append(options, agent.RegistryOption{
			Intent:  domain.PlanIntentTask,
			Tool:    tool,
			Encode:  echoPlanTitle,
			Default: i == 0,
		})
	}

	registry, err := agent.NewRegistry(options...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	service, err := agent.NewService(store, p, registry, agent.Options{Clock: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// newServiceWithBindings builds a service over an explicit registry, for the tests
// that care which mechanism a plan resolves to.
func newServiceWithBindings(t *testing.T, store *fakeStore, p *fakePlanner, options ...agent.RegistryOption) *agent.Service {
	t.Helper()

	registry, err := agent.NewRegistry(options...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	service, err := agent.NewService(store, p, registry, agent.Options{Clock: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	return service
}

// echoPlanTitle renders a plan as {"title": ...}, which every fake tool accepts.
func echoPlanTitle(plan *planner.Plan) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"title": plan.Title})
}

func taskTool() *fakeTool {
	return &fakeTool{name: "tasks.create"}
}

// The run has to exist before the model is consulted, otherwise a crash or a
// disconnect during planning leaves nothing to find.
func TestPlanPersistsTheRunBeforePlanning(t *testing.T) {
	store := newFakeStore()

	order := []string{}
	p := &fakePlanner{outcome: goodPlan()}
	p.onPlan = func(_ context.Context) {
		order = append(order, "plan")
	}

	service := newService(t, store, p, taskTool())

	if _, err := service.Plan(context.Background(), uuid.New(), "buy milk"); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// CreateRun must precede the model call, which the fake records in order.
	createIndex := indexOf(store.calls, "CreateRun")
	if createIndex < 0 {
		t.Fatalf("CreateRun was never called: %v", store.calls)
	}

	if len(order) != 1 {
		t.Fatalf("planner was not consulted")
	}

	if store.calls[0] != "CreateRun" {
		t.Errorf("first write = %q, want CreateRun first", store.calls[0])
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}

	return -1
}

func TestPlanRecordsWhetherThePlanWasAFallback(t *testing.T) {
	// A fallback plan is the user's own goal turned into a title, so it reads like
	// a considered plan. A client has to be able to tell them apart.
	fallback := goodPlan()
	fallback.UsedFallback = true

	tests := map[string]struct {
		outcome *planner.Outcome
		want    bool
	}{
		"model produced it": {outcome: goodPlan(), want: false},
		"fallback":          {outcome: fallback, want: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()

			run, err := newService(t, store, &fakePlanner{outcome: tt.outcome}, taskTool()).
				Plan(context.Background(), uuid.New(), "buy milk")
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			if run.Run.PlanFallback != tt.want {
				t.Errorf("PlanFallback = %v, want %v", run.Run.PlanFallback, tt.want)
			}
		})
	}
}

// A planning timeout must leave a failed run behind, not a salvaged one. Before
// this was fixed the planner fell back on timeout and the run reached
// awaiting_approval looking like any other plan.
func TestPlanRecordsAFailedRunWhenPlanningTimesOut(t *testing.T) {
	store := newFakeStore()

	_, err := newService(t, store, &fakePlanner{err: context.DeadlineExceeded}, taskTool()).
		Plan(context.Background(), uuid.New(), "buy milk")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline to reach the caller", err)
	}

	run := store.onlyRun(t)
	if run.Status != domain.RunStatusFailed {
		t.Errorf("Status = %q, want failed", run.Status)
	}

	if run.Error == "" {
		t.Error("Error is empty: the reason would be lost once the client disconnects")
	}

	if run.FinishedAt == nil {
		t.Error("FinishedAt was not stamped on a terminal run")
	}

	if run.PlanFallback {
		t.Error("PlanFallback = true on a run that never got a plan")
	}
}

// The run has to carry the trace id so it can be found in the tracing backend
// starting from the database. Telemetry is off by default, so this builds the
// span context directly rather than relying on a configured provider.
func TestPlanRecordsTheTraceIDFromTheRequestSpan(t *testing.T) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})

	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)

	run, err := newService(t, newFakeStore(), &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(ctx, uuid.New(), "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if run.Run.TraceID != spanContext.TraceID().String() {
		t.Errorf("TraceID = %q, want %q", run.Run.TraceID, spanContext.TraceID().String())
	}
}

// With no span active there is nothing to record, and the column is nullable for
// exactly this case.
func TestPlanLeavesTheTraceIDEmptyWithoutASpan(t *testing.T) {
	run, err := newService(t, newFakeStore(), &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(context.Background(), uuid.New(), "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if run.Run.TraceID != "" {
		t.Errorf("TraceID = %q, want empty when tracing is disabled", run.Run.TraceID)
	}
}

func TestPlanProducesOneApprovalAwaitingApproval(t *testing.T) {
	store := newFakeStore()
	tool := taskTool()

	// The real task encoder rather than the shared stub: this test asserts the
	// payload a user is actually shown, so a stub that only carries the title would
	// make it pass for the wrong reason.
	service := newServiceWithBindings(t, store, &fakePlanner{outcome: goodPlan()},
		agent.RegistryOption{
			Intent:  domain.PlanIntentTask,
			Tool:    tool,
			Encode:  agent.EncodeTaskCreateArguments,
			Default: true,
		})

	run, err := service.Plan(context.Background(), uuid.New(), "  buy milk  ")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if run.Run.Status != domain.RunStatusAwaitingApproval {
		t.Errorf("Status = %q, want awaiting_approval", run.Run.Status)
	}

	if run.Run.Goal != "buy milk" {
		t.Errorf("Goal = %q, want it trimmed", run.Run.Goal)
	}

	if run.Run.StartedAt == nil {
		t.Error("StartedAt was not stamped")
	}

	if run.Run.FinishedAt != nil {
		t.Error("FinishedAt is set on a run that is still waiting for approval")
	}

	if len(run.Approvals) != 1 {
		t.Fatalf("Approvals = %d, want 1", len(run.Approvals))
	}

	approval := run.Approvals[0]

	if approval.ToolName != "tasks.create" {
		t.Errorf("ToolName = %q", approval.ToolName)
	}

	if approval.Status != domain.ApprovalStatusPending {
		t.Errorf("Approval status = %q, want pending", approval.Status)
	}

	// The stored arguments have to be executable, because they are replayed
	// verbatim on approval.
	if err := tool.Validate(approval.Arguments); err != nil {
		t.Errorf("stored arguments would not execute: %v", err)
	}

	var payload struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
	}

	if err := json.Unmarshal(approval.Arguments, &payload); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}

	if payload.Title != "Buy semi-skimmed milk" {
		t.Errorf("title = %q", payload.Title)
	}

	if payload.Priority != "high" {
		t.Errorf("priority = %q, want high", payload.Priority)
	}

	// Steps belong in the description rather than becoming separate tasks.
	if !strings.Contains(payload.Description, "1. Check the fridge") {
		t.Errorf("description does not carry the steps: %q", payload.Description)
	}
}

// Planning runs on a context detached from the request, so a client that hangs
// up does not throw away a ten second model call: the run still reaches a usable
// state and can be approved whenever the user comes back for it.
func TestPlanCompletesEvenWhenTheCallerDisconnects(t *testing.T) {
	store := newFakeStore()

	ctx, cancel := context.WithCancel(context.Background())

	p := &fakePlanner{outcome: goodPlan()}
	p.onPlan = func(_ context.Context) {
		cancel()
	}

	planned, err := newService(t, store, p, taskTool()).Plan(ctx, uuid.New(), "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if planned.Run.Status != domain.RunStatusAwaitingApproval {
		t.Errorf("Status = %q, want awaiting_approval rather than a stranded run", planned.Run.Status)
	}

	if len(planned.Approvals) != 1 {
		t.Errorf("approvals = %d, want the plan still proposed for review", len(planned.Approvals))
	}

	if err := ctx.Err(); err == nil {
		t.Error("the request context was expected to be cancelled by this point")
	}
}

func TestPlanRecordsAPlannerFailureOnTheRun(t *testing.T) {
	store := newFakeStore()

	_, err := newService(t, store, &fakePlanner{err: errors.New("model exploded")}, taskTool()).
		Plan(context.Background(), uuid.New(), "buy milk")
	if err == nil {
		t.Fatal("Plan returned no error")
	}

	if len(store.runs) != 1 {
		t.Fatalf("runs stored = %d, want 1", len(store.runs))
	}

	for _, run := range store.runs {
		if run.Status != domain.RunStatusFailed {
			t.Errorf("Status = %q, want failed", run.Status)
		}

		if !strings.Contains(run.Error, "model exploded") {
			t.Errorf("Error = %q, want the planner cause", run.Error)
		}
	}
}

// The user is never shown arguments that would be refused at execution time.
func TestPlanRefusesToProposeUnusableArguments(t *testing.T) {
	store := newFakeStore()
	tool := taskTool()
	tool.validateErr = errors.New("title is required")

	if _, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Plan(context.Background(), uuid.New(), "buy milk"); err == nil {
		t.Fatal("Plan proposed arguments its own tool rejects")
	}

	if len(store.approvals) != 0 {
		t.Errorf("approvals created = %d, want 0", len(store.approvals))
	}

	for _, run := range store.runs {
		if run.Status != domain.RunStatusFailed {
			t.Errorf("Status = %q, want failed", run.Status)
		}
	}
}

func TestApproveExecutesOnceAndCompletesTheRun(t *testing.T) {
	store := newFakeStore()
	tool := taskTool()

	owner := uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	approvalID := planned.Approvals[0].ID

	approved, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Approve(context.Background(), owner, approvalID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if approved.Run.Status != domain.RunStatusCompleted {
		t.Errorf("Status = %q, want completed", approved.Run.Status)
	}

	if approved.Run.Result == nil {
		t.Error("Result was not recorded")
	}

	if approved.Run.FinishedAt == nil {
		t.Error("FinishedAt was not stamped")
	}

	if len(tool.executions) != 1 {
		t.Fatalf("tool executions = %d, want 1", len(tool.executions))
	}

	// The tool must run as the owner, against the run it belongs to.
	if tool.userIDs[0] != owner {
		t.Error("the tool did not run as the run owner")
	}

	if tool.runIDs[0] != planned.Run.ID {
		t.Error("the tool was not given the run id")
	}
}

// Approving twice must not create a second task, so the approval is claimed
// before the tool runs.
func TestApproveIsNotRepeatable(t *testing.T) {
	store := newFakeStore()
	tool := taskTool()

	owner := uuid.New()
	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	service := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool)

	if _, err := service.Approve(context.Background(), owner, planned.Approvals[0].ID); err != nil {
		t.Fatalf("first Approve: %v", err)
	}

	_, err = service.Approve(context.Background(), owner, planned.Approvals[0].ID)
	if !errors.Is(err, domain.ErrStateTransition) {
		t.Errorf("second Approve err = %v, want ErrStateTransition", err)
	}

	if len(tool.executions) != 1 {
		t.Errorf("tool executions = %d, want 1 after a repeated approval", len(tool.executions))
	}
}

// Rejecting ends the whole run: continuing after the user has said no to part of
// a plan would mean deciding what the rest is.
func TestRejectCancelsTheWholeRun(t *testing.T) {
	store := newFakeStore()

	owner := uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	rejected, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Reject(context.Background(), owner, planned.Approvals[0].ID)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if rejected.Run.Status != domain.RunStatusCancelled {
		t.Errorf("Status = %q, want cancelled", rejected.Run.Status)
	}

	if rejected.Approvals[0].Status != domain.ApprovalStatusRejected {
		t.Errorf("approval status = %q, want rejected", rejected.Approvals[0].Status)
	}

	if rejected.Run.Result != nil {
		t.Error("a cancelled run recorded a result")
	}
}

func TestApproveFailsTheRunWhenTheToolFails(t *testing.T) {
	store := newFakeStore()
	tool := taskTool()
	tool.executeErr = errors.New("database is down")

	owner := uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	_, err = newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Approve(context.Background(), owner, planned.Approvals[0].ID)
	if err == nil {
		t.Fatal("Approve reported success despite the tool failing")
	}

	stored, err := store.GetRun(context.Background(), owner, planned.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if stored.Status != domain.RunStatusFailed {
		t.Errorf("Status = %q, want failed", stored.Status)
	}

	if !strings.Contains(stored.Error, "database is down") {
		t.Errorf("Error = %q, want the tool cause", stored.Error)
	}
}

// Another user must not be able to approve, cancel or read a run.
func TestAnotherUserCannotReachTheRun(t *testing.T) {
	store := newFakeStore()

	owner := uuid.New()
	intruder := uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	service := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool())

	if _, err := service.Get(context.Background(), intruder, planned.Run.ID); !errors.Is(err, errNotFound) {
		t.Errorf("Get err = %v, want not found", err)
	}

	if _, err := service.Approve(context.Background(), intruder, planned.Approvals[0].ID); !errors.Is(err, errNotFound) {
		t.Errorf("Approve err = %v, want not found", err)
	}

	if _, err := service.Reject(context.Background(), intruder, planned.Approvals[0].ID); !errors.Is(err, errNotFound) {
		t.Errorf("Reject err = %v, want not found", err)
	}

	if _, err := service.Cancel(context.Background(), intruder, planned.Run.ID); !errors.Is(err, errNotFound) {
		t.Errorf("Cancel err = %v, want not found", err)
	}
}

func TestCancelStopsALiveRunAndRefusesAFinishedOne(t *testing.T) {
	store := newFakeStore()

	owner := uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	cancelled, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Cancel(context.Background(), owner, planned.Run.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if cancelled.Run.Status != domain.RunStatusCancelled {
		t.Errorf("Status = %q, want cancelled", cancelled.Run.Status)
	}

	_, err = newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Cancel(context.Background(), owner, planned.Run.ID)
	if !errors.Is(err, domain.ErrStateTransition) {
		t.Errorf("second Cancel err = %v, want ErrStateTransition", err)
	}
}

// A run must not be left awaiting approval with nothing to approve. The guarantee
// used to be "the tool the plan needs must be registered"; it is now "the registry
// must be able to produce an executable proposal", which can still fail — so it is
// checked here rather than assumed.
func TestPlanFailsWhenTheProposalCouldNotBeBuilt(t *testing.T) {
	store := newFakeStore()

	failing := func(*planner.Plan) (json.RawMessage, error) {
		return nil, errors.New("could not encode tool arguments")
	}

	_, err := newServiceWithBindings(t, store, &fakePlanner{outcome: goodPlan()},
		agent.RegistryOption{
			Intent:  domain.PlanIntentTask,
			Tool:    taskTool(),
			Encode:  failing,
			Default: true,
		}).Plan(context.Background(), uuid.New(), "buy milk")
	if err == nil {
		t.Fatal("Plan succeeded even though the proposal could not be built")
	}

	// The run row exists from the moment planning starts, so it has to record why.
	if !containsCall(store.calls, "UpdateRun") {
		t.Errorf("calls = %v, want the run's failure to be recorded", store.calls)
	}
}

func containsCall(calls []string, want string) bool {
	return indexOf(calls, want) >= 0
}

func TestPlanRejectsABlankGoal(t *testing.T) {
	store := newFakeStore()

	_, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, taskTool()).
		Plan(context.Background(), uuid.New(), "   ")
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}

	if len(store.runs) != 0 {
		t.Errorf("runs stored = %d, want 0", len(store.runs))
	}
}

func TestNewServiceValidatesItsDependencies(t *testing.T) {
	store := newFakeStore()
	p := &fakePlanner{outcome: goodPlan()}

	if _, err := agent.NewService(nil, p, nil, agent.Options{}); err == nil {
		t.Error("NewService accepted a nil store")
	}

	if _, err := agent.NewService(store, nil, nil, agent.Options{}); err == nil {
		t.Error("NewService accepted a nil planner")
	}

	if _, err := agent.NewService(store, p, nil, agent.Options{}); err == nil {
		t.Error("NewService accepted a nil registry")
	}
}

// The registry is where a plan becomes a mechanism, so the things that can go
// wrong there have to be refused at construction: they cannot be repaired later.
func TestNewRegistryValidatesItsBindings(t *testing.T) {
	task := taskTool()

	tests := map[string]struct {
		options []agent.RegistryOption
		wantErr string
	}{
		"nil tool": {
			options: []agent.RegistryOption{{Intent: domain.PlanIntentTask, Tool: nil, Encode: echoPlanTitle, Default: true}},
			wantErr: "nil tool",
		},
		"no encoder": {
			options: []agent.RegistryOption{{Intent: domain.PlanIntentTask, Tool: task, Default: true}},
			wantErr: "no argument encoder",
		},
		"duplicate tool": {
			options: []agent.RegistryOption{
				{Tool: task, Encode: echoPlanTitle, Default: true},
				{Tool: taskTool(), Encode: echoPlanTitle},
			},
			wantErr: "duplicate tool",
		},
		"duplicate intent": {
			options: []agent.RegistryOption{
				{Intent: domain.PlanIntentTask, Tool: task, Encode: echoPlanTitle, Default: true},
				{Intent: domain.PlanIntentTask, Tool: &fakeTool{name: "other"}, Encode: echoPlanTitle},
			},
			wantErr: "claimed by more than one tool",
		},
		"two defaults": {
			options: []agent.RegistryOption{
				{Intent: domain.PlanIntentTask, Tool: task, Encode: echoPlanTitle, Default: true},
				{Intent: domain.PlanIntentCalendarEvent, Tool: &fakeTool{name: "calendar.create_event"}, Encode: echoPlanTitle, Default: true},
			},
			wantErr: "more than one default",
		},
		"no default": {
			options: []agent.RegistryOption{
				{Intent: domain.PlanIntentTask, Tool: task, Encode: echoPlanTitle},
			},
			wantErr: "default binding is required",
		},
		"read-only tool cannot back a proposal": {
			options: []agent.RegistryOption{
				{Intent: domain.PlanIntentTask, Tool: &fakeTool{name: "calendar.list_events", readOnly: true}, Encode: echoPlanTitle, Default: true},
			},
			wantErr: "cannot back a proposal",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := agent.NewRegistry(tt.options...)
			if err == nil {
				t.Fatalf("NewRegistry accepted %s", name)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// An unrecognised intent must fall back to the default rather than to a guess at a
// mutating external service. This is the rule that stops the planner reaching a
// connector it was not asked about.
func TestUnrecognisedIntentFallsBackToTheDefaultTool(t *testing.T) {
	task := taskTool()
	calendar := &fakeTool{name: "calendar.create_event"}

	registry, err := agent.NewRegistry(
		agent.RegistryOption{Intent: domain.PlanIntentTask, Tool: task, Encode: echoPlanTitle, Default: true},
		agent.RegistryOption{Intent: domain.PlanIntentCalendarEvent, Tool: calendar, Encode: echoPlanTitle},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	proposal, err := registry.Propose(&planner.Plan{Intent: domain.PlanIntent("invent_something_else"), Title: "Buy milk"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if proposal.Tool.Name() != "tasks.create" {
		t.Errorf("tool = %q, want the default tasks.create", proposal.Tool.Name())
	}

	if len(calendar.executions) != 0 {
		t.Error("the connector tool was selected for an unrecognised intent")
	}
}

// The intent a plan carries has to reach the proposal, or the selector is guessing
// rather than resolving.
func TestIntentSelectsItsOwnTool(t *testing.T) {
	task := taskTool()
	calendar := &fakeTool{name: "calendar.create_event"}

	registry, err := agent.NewRegistry(
		agent.RegistryOption{Intent: domain.PlanIntentTask, Tool: task, Encode: echoPlanTitle, Default: true},
		agent.RegistryOption{Intent: domain.PlanIntentCalendarEvent, Tool: calendar, Encode: echoPlanTitle},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	proposal, err := registry.Propose(&planner.Plan{Intent: domain.PlanIntentCalendarEvent, Title: "Deploy freeze"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if proposal.Tool.Name() != "calendar.create_event" {
		t.Errorf("tool = %q, want calendar.create_event", proposal.Tool.Name())
	}

	// The payload must not be re-derived per tool: it is the plan's own content.
	var arguments map[string]string
	if err := json.Unmarshal(proposal.Arguments, &arguments); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}

	if arguments["title"] != "Deploy freeze" {
		t.Errorf("arguments = %v, want the plan's title", arguments)
	}
}

// Validation runs inside the registry so a payload that would be refused at
// execution is never shown for approval.
func TestProposeRefusesArgumentsTheToolWouldReject(t *testing.T) {
	tool := taskTool()
	tool.validateErr = errors.New("title is required")

	registry, err := agent.NewRegistry(agent.RegistryOption{
		Intent: domain.PlanIntentTask, Tool: tool, Encode: echoPlanTitle, Default: true,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	_, err = registry.Propose(&planner.Plan{Intent: domain.PlanIntentTask, Title: "Buy milk"})
	if err == nil {
		t.Fatal("Propose succeeded with arguments the tool refuses")
	}

	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error = %q, want it to say the arguments are not executable", err)
	}
}

// The real tool has to agree with the arguments the service produces.
func TestTaskCreateToolRoundTrip(t *testing.T) {
	creations := make([]task.CreateInput, 0)

	creator := creatorFunc(func(_ context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error) {
		creations = append(creations, input)

		return &domain.Task{ID: uuid.New(), UserID: userID, Title: input.Title}, nil
	})

	tool := agent.NewTaskCreateTool(creator)

	arguments, err := json.Marshal(map[string]any{
		"title":       "Buy milk",
		"description": "1. Check the fridge",
		"priority":    "high",
		"due_at":      "2026-10-09T18:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := tool.Validate(arguments); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	runID := uuid.New()

	if _, err := tool.Execute(context.Background(), uuid.New(), runID, arguments); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(creations) != 1 {
		t.Fatalf("creations = %d, want 1", len(creations))
	}

	created := creations[0]

	if created.Source != "agent" {
		t.Errorf("Source = %q, want agent so the task is identifiable", created.Source)
	}

	if created.RunID == nil || *created.RunID != runID {
		t.Error("the task was not linked back to its run")
	}

	if created.DueAt == nil || created.DueAt.Hour() != 18 {
		t.Errorf("DueAt = %v, want 18:00 parsed", created.DueAt)
	}
}

func TestTaskCreateToolRejectsUnusableArguments(t *testing.T) {
	tool := agent.NewTaskCreateTool(creatorFunc(func(_ context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error) {
		return &domain.Task{ID: uuid.New(), UserID: userID}, nil
	}))

	cases := map[string]string{
		"blank title":      `{"title":"   "}`,
		"unknown priority": `{"title":"Buy milk","priority":"urgent"}`,
		"bad timestamp":    `{"title":"Buy milk","due_at":"friday"}`,
		"unknown field":    `{"title":"Buy milk","run_id":"00000000-0000-0000-0000-000000000000"}`,
		"not an object":    `"buy milk"`,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tool.Validate(json.RawMessage(raw)); err == nil {
				t.Errorf("Validate accepted %s", raw)
			}
		})
	}
}

type creatorFunc func(ctx context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error)

func (f creatorFunc) Create(ctx context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error) {
	return f(ctx, userID, input)
}

// A grounded plan and an ungrounded one are the same plan as far as the user can
// see, so the difference has to survive onto the run.
func TestRunRecordsHowThePlanWasGrounded(t *testing.T) {
	for _, want := range []domain.GroundingState{
		domain.GroundingGrounded,
		domain.GroundingNoContext,
		domain.GroundingUngrounded,
	} {
		t.Run(string(want), func(t *testing.T) {
			outcome := goodPlan()
			outcome.Grounding = want

			store := newFakeStore()
			service := newService(t, store, &fakePlanner{outcome: outcome}, taskTool())
			userID := uuid.New()

			created, err := service.Plan(context.Background(), userID, "buy milk")
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			if created.Run.GroundingState != want {
				t.Errorf("GroundingState = %q, want %q", created.Run.GroundingState, want)
			}

			// Persisted, not just returned, because the run is what a later read of
			// the run has to describe.
			stored, err := store.GetRun(context.Background(), userID, created.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}

			if stored.GroundingState != want {
				t.Errorf("stored GroundingState = %q, want %q", stored.GroundingState, want)
			}
		})
	}
}

// The default has to be the honest one: nothing retrieved yet is not the same as
// nothing to retrieve.
func TestANewRunIsNotAssumedGrounded(t *testing.T) {
	run := &domain.AgentRun{}

	if run.GroundingState == domain.GroundingGrounded {
		t.Error("a run with no plan claims it was grounded")
	}
}
