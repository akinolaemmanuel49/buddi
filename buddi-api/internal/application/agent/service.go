// Package agent turns a goal into reviewed, executed changes.
//
// A run is a durable record with exactly one lifecycle: plan, ask, execute. The
// model proposes; nothing it produces is applied without a user decision. The
// step that actually changes something sits behind the Tool interface so that
// today's in-process implementation can be swapped for an MCP client later
// without touching the orchestration around it.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/observability"
)

// Tool is one action the agent may take on the user's behalf.
//
// Arguments arrive as raw JSON because that is exactly what gets shown to the
// user for approval and stored on the approval row. The bytes that are reviewed
// have to be the bytes that run, so nothing re-serialises them in between.
type Tool interface {
	// Name is the stable identifier stored on the approval.
	Name() string

	// Description explains the tool to the user and the model.
	Description() string

	// Mutating reports whether executing this tool changes something. Every
	// approval proposes a change, so the registry will not resolve a plan to a tool
	// that does not mutate: there would be nothing for the user to approve.
	Mutating() bool

	// Validate checks arguments before they are proposed for approval, so the
	// user is never shown something that would be rejected on execution.
	Validate(arguments json.RawMessage) error

	// Execute performs the action.
	Execute(ctx context.Context, userID uuid.UUID, runID uuid.UUID, arguments json.RawMessage) (json.RawMessage, error)
}

// Planner produces a plan for a goal.
type Planner interface {
	// Plan takes the requesting user so the plan can be grounded in that user's own
	// notes. Without it the only groundable scope would be "every note in the
	// system", which is not a scope anyone would accept.
	Plan(ctx context.Context, userID uuid.UUID, goal string) (*planner.Outcome, error)
}

// RunStore persists runs and their approvals.
type RunStore interface {
	CreateRun(ctx context.Context, run *domain.AgentRun) error
	UpdateRun(ctx context.Context, run *domain.AgentRun) error
	GetRun(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*domain.AgentRun, error)
	ListRuns(ctx context.Context, userID uuid.UUID, filter domain.RunFilter, page domain.Page) ([]domain.AgentRun, int64, error)

	CreateApproval(ctx context.Context, approval *domain.Approval) error
	UpdateApproval(ctx context.Context, approval *domain.Approval) error
	GetApproval(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*domain.Approval, error)
	ListApprovals(ctx context.Context, userID uuid.UUID, runID uuid.UUID) ([]domain.Approval, error)
}

// Run pairs a run with its approvals for reading.
type Run struct {
	Run       *domain.AgentRun
	Approvals []domain.Approval
}

// Options configures the Service.
type Options struct {
	// PlanTimeout bounds planning. The API answers this request synchronously, so
	// a hung model would otherwise hold the connection open indefinitely: Go's
	// server sets no handler deadline, and its write timeout would abandon the
	// response without recording the outcome.
	PlanTimeout time.Duration

	// ToolTimeout bounds a single tool execution.
	ToolTimeout time.Duration

	// Clock is injectable for deterministic tests.
	Clock func() time.Time
}

// Service orchestrates runs.
type Service struct {
	store    RunStore
	planner  Planner
	registry *Registry

	planTimeout time.Duration
	toolTimeout time.Duration
	now         func() time.Time
}

// NewService builds the agent service. Every registered tool must have a unique
// name, because the name is what an approval refers to.
func NewService(store RunStore, planner Planner, registry *Registry, opts Options) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("agent: run store is required")
	}

	if planner == nil {
		return nil, fmt.Errorf("agent: planner is required")
	}

	if registry == nil {
		return nil, fmt.Errorf("agent: tool registry is required")
	}

	service := &Service{
		store:       store,
		planner:     planner,
		registry:    registry,
		planTimeout: opts.PlanTimeout,
		toolTimeout: opts.ToolTimeout,
		now:         opts.Clock,
	}

	if service.now == nil {
		service.now = time.Now
	}

	if service.planTimeout <= 0 {
		service.planTimeout = 45 * time.Second
	}

	if service.toolTimeout <= 0 {
		service.toolTimeout = 30 * time.Second
	}

	return service, nil
}

// Plan creates a run, plans it, and stops for approval.
//
// The run row is written before the model is called so that a crash, a timeout
// or a client disconnect leaves a record to find rather than a silently missing
// run. Finalisation deliberately runs on a context detached from the request: if
// the caller hangs up mid-plan, the run still has to reach a terminal state.
func (s *Service) Plan(ctx context.Context, userID uuid.UUID, goal string) (*Run, error) {
	run, err := s.begin(ctx, userID, goal)
	if err != nil {
		return nil, err
	}

	outcome, err := s.planWithTimeout(run.finaliseCtx, userID, run.domain.Goal)
	if err != nil {
		// A planning failure is recorded on the run rather than only returned, so
		// the reason survives for a client that is no longer listening.
		s.failRun(run.domain, err)

		return nil, err
	}

	return s.finish(run, userID, outcome)
}

// PlanFromOutcome records a run for a plan that has already been produced.
//
// It exists for the chat surface, which streams the plan to the user as it is
// generated and must not pay for it twice. On a CPU runtime one planning call is
// the dominant cost of a turn, so re-planning to obtain the same plan would double
// the latency of every planned message.
//
// The outcome is trusted to have come from the planner, which is the same trust the
// caller already places in the port. It is not validated again because it was
// validated when it was produced.
func (s *Service) PlanFromOutcome(
	ctx context.Context,
	userID uuid.UUID,
	goal string,
	outcome *planner.Outcome,
) (*Run, error) {
	run, err := s.begin(ctx, userID, goal)
	if err != nil {
		return nil, err
	}

	return s.finish(run, userID, outcome)
}

// pendingRun is a run row that exists but has not been given a plan yet.
//
// It carries the detached context alongside the row because finalisation must
// survive the client hanging up, and the run and its finalisation state have to
// stay together: pairing a run with the wrong context is how a run ends up
// permanently stuck in "planning".
type pendingRun struct {
	domain *domain.AgentRun

	finaliseCtx context.Context
}

// begin creates the run row and moves it into planning.
//
// The row is written before the model is called so that a crash, a timeout or a
// client disconnect leaves a record to find rather than a silently missing run.
// Everything past this point runs on a context detached from the request.
func (s *Service) begin(ctx context.Context, userID uuid.UUID, goal string) (*pendingRun, error) {
	now := s.now()

	run, err := domain.NewAgentRun(userID, goal, now)
	if err != nil {
		return nil, err
	}

	// Taken from the request context so the run can be found in the tracing
	// backend from the database. Empty when tracing is disabled, which is why the
	// column is nullable.
	run.TraceID = observability.TraceIDFromContext(ctx)

	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}

	// Everything past this point must be able to finish without the client.
	finalise := context.WithoutCancel(ctx)

	if err := run.StartPlanning(now); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(finalise, run); err != nil {
		return nil, err
	}

	return &pendingRun{domain: run, finaliseCtx: finalise}, nil
}

// finish records a plan against a pending run and parks it for approval.
func (s *Service) finish(pending *pendingRun, userID uuid.UUID, outcome *planner.Outcome) (*Run, error) {
	run := pending.domain

	if outcome == nil || outcome.Plan == nil {
		err := fmt.Errorf("agent: planner returned no plan")
		s.failRun(run, err)

		return nil, err
	}

	if err := s.propose(pending.finaliseCtx, run, outcome, userID); err != nil {
		s.failRun(run, err)

		return nil, err
	}

	if err := run.AwaitApproval(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(pending.finaliseCtx, run); err != nil {
		return nil, err
	}

	return s.load(pending.finaliseCtx, userID, run.ID)
}

// planWithTimeout bounds the model call.
func (s *Service) planWithTimeout(ctx context.Context, userID uuid.UUID, goal string) (*planner.Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, s.planTimeout)
	defer cancel()

	outcome, err := s.planner.Plan(ctx, userID, goal)
	if err != nil {
		return nil, err
	}

	if outcome == nil || outcome.Plan == nil {
		return nil, fmt.Errorf("agent: planner returned no plan")
	}

	return outcome, nil
}

// propose turns a plan into an approval the user can accept or decline.
//
// One plan becomes one proposal. Which mechanism carries it out is the registry's
// decision, not the model's and not a hardcoded name here: see Registry.Propose.
// A plan with several steps still becomes a single proposal, which keeps one plan to
// one approval.
func (s *Service) propose(ctx context.Context, run *domain.AgentRun, outcome *planner.Outcome, userID uuid.UUID) error {
	plan := outcome.Plan

	// Resolved and validated before the proposal is shown, so the user is never
	// shown arguments that would be refused at execution time.
	proposal, err := s.registry.Propose(plan)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("agent: could not encode plan: %w", err)
	}

	run.Plan = encoded
	run.Model = outcome.Model
	// Persisted so a client can tell a planned run from a salvaged one. See
	// domain.AgentRun.PlanFallback for why this is not a detail.
	run.PlanFallback = outcome.UsedFallback
	// Same reasoning as PlanFallback: the plan looks the same whether or not the
	// user's notes reached the model, so the difference has to be stored.
	run.GroundingState = outcome.Grounding

	rationale := proposal.Rationale
	if rationale == "" {
		rationale = proposal.Tool.Description()
	}

	approval, err := domain.NewApproval(run.ID, userID, 0, proposal.Tool.Name(), proposal.Arguments, rationale, s.now())
	if err != nil {
		return err
	}

	return s.store.CreateApproval(ctx, approval)
}

// Approve executes a single approved proposal and completes the run.
func (s *Service) Approve(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*Run, error) {
	approval, err := s.store.GetApproval(ctx, userID, approvalID)
	if err != nil {
		return nil, err
	}

	run, err := s.store.GetRun(ctx, userID, approval.RunID)
	if err != nil {
		return nil, err
	}

	// The approval is claimed before the tool runs. If two approvals race, the
	// second sees a non-pending approval and is refused rather than duplicating
	// the mutation.
	if err := approval.Approve(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateApproval(ctx, approval); err != nil {
		return nil, err
	}

	if run.Status != domain.RunStatusAwaitingApproval {
		return nil, domain.ErrStateTransition
	}

	if err := run.BeginExecuting(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}

	tool, ok := s.registry.Binding(approval.ToolName)
	if !ok {
		return nil, s.failRunWith(ctx, run, fmt.Errorf("agent: unknown tool %q", approval.ToolName))
	}

	toolCtx, cancel := context.WithTimeout(ctx, s.toolTimeout)
	defer cancel()

	// The stored arguments are replayed verbatim: what was reviewed is what runs.
	result, err := tool.Tool.Execute(toolCtx, userID, run.ID, approval.Arguments)
	if err != nil {
		failure := fmt.Errorf("agent: %s failed: %w", tool.Tool.Name(), err)

		if IsTransientToolError(err) {
			return s.retryable(ctx, userID, run, approval, failure)
		}

		return nil, s.failRunWith(ctx, run, failure)
	}

	run.Result = result

	if err := run.Complete(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}

	return s.load(ctx, userID, run.ID)
}

// IsTransientToolError reports whether a tool failure is worth retrying unchanged.
//
// It is deliberately conservative, because retrying a mutation is not free: if the
// tool did its work and only the response was lost, a second attempt writes the same
// event twice. So only failures that mean the call demonstrably never reached its
// destination are treated as transient, and everything else consumes the approval and
// fails the run.
//
// A provider that has accepted a request and then errored is not detectable from here,
// which is why the classification errs towards "not transient".
func IsTransientToolError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	// A transport-level failure: the connection was refused, reset, or never opened.
	// Distinguished from an application-level error, which is a normal response from
	// a service that understood the request and declined it.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// A credential that has to be renewed is a caller problem, not a decision problem,
	// and re-deciding does not fix it.
	if errors.Is(err, oauth.ErrRevoked) || errors.Is(err, oauth.ErrNotConnected) {
		return true
	}

	return false
}

// retryable puts a run and its approval back the way they were before execution, so
// the user can approve the same thing again.
//
// Both records are unwound, not just the approval: Approve refuses a run that is not
// awaiting approval, so releasing the approval alone would leave the button visible and
// the second click rejected.
//
// The run is returned rather than an error. Nothing has been lost and the decision
// still stands, so reporting a failure would tell the user their approval was rejected
// when what happened is that the call did not complete. The reason stays on the run so
// the UI can explain itself.
func (s *Service) retryable(
	ctx context.Context,
	userID uuid.UUID,
	run *domain.AgentRun,
	approval *domain.Approval,
	reason error,
) (*Run, error) {
	// Detached from the request: the tool call may have timed out because the user
	// closed the tab, and the records still have to be put back.
	finalise := context.WithoutCancel(ctx)

	now := s.now()

	if err := run.Retry(now, reason.Error()); err != nil {
		return nil, s.failRunWith(finalise, run, reason)
	}

	if err := approval.Release(now); err != nil {
		return nil, s.failRunWith(finalise, run, reason)
	}

	// The approval is written first so the two cannot disagree about which one happened:
	// a pending approval on a still-executing run would look approved with nothing run.
	if err := s.store.UpdateApproval(finalise, approval); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(finalise, run); err != nil {
		return nil, err
	}

	return s.load(finalise, userID, run.ID)
}

// Reject declines a proposal. Declining one ends the whole run: a plan is a
// coherent unit, and continuing after the user has said no to part of it would
// mean deciding what "the rest" is, which is exactly the decision tree this
// deliberately avoids.
func (s *Service) Reject(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*Run, error) {
	approval, err := s.store.GetApproval(ctx, userID, approvalID)
	if err != nil {
		return nil, err
	}

	run, err := s.store.GetRun(ctx, userID, approval.RunID)
	if err != nil {
		return nil, err
	}

	if err := approval.Reject(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateApproval(ctx, approval); err != nil {
		return nil, err
	}

	if err := run.Cancel(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}

	return s.load(ctx, userID, run.ID)
}

// Get returns one run with its approvals.
func (s *Service) Get(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*Run, error) {
	if _, err := s.store.GetRun(ctx, userID, runID); err != nil {
		return nil, err
	}

	return s.load(ctx, userID, runID)
}

// List returns the caller's runs, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, filter domain.RunFilter, page domain.Page) (*RunList, error) {
	runs, total, err := s.store.ListRuns(ctx, userID, filter, page)
	if err != nil {
		return nil, err
	}

	return &RunList{Runs: runs, Total: total, Page: page}, nil
}

// Cancel stops a run that has not finished.
func (s *Service) Cancel(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*Run, error) {
	run, err := s.store.GetRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}

	if err := run.Cancel(s.now()); err != nil {
		return nil, err
	}

	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}

	return s.load(ctx, userID, runID)
}

// RunList is a page of runs.
type RunList struct {
	Runs  []domain.AgentRun
	Total int64
	Page  domain.Page
}

// load assembles a run with its approvals.
func (s *Service) load(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*Run, error) {
	run, err := s.store.GetRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}

	approvals, err := s.store.ListApprovals(ctx, userID, runID)
	if err != nil {
		return nil, err
	}

	return &Run{Run: run, Approvals: approvals}, nil
}

// failRun records a failure on a detached context and returns the original error
// so the caller still sees what went wrong.
func (s *Service) failRun(run *domain.AgentRun, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	// Best effort: the caller is getting the real error either way, and there is
	// nothing useful to do with a failure to persist the failure.
	_ = s.failRunWith(ctx, run, cause)

	return cause
}

func (s *Service) failRunWith(ctx context.Context, run *domain.AgentRun, cause error) error {
	if err := run.Fail(s.now(), cause.Error()); err != nil {
		// A run that has already finished cannot be failed, which is not itself a
		// problem worth masking the cause with.
		return cause
	}

	if err := s.store.UpdateRun(ctx, run); err != nil {
		return cause
	}

	return cause
}
