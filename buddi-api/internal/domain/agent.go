package domain

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunStatus tracks an agent run through its lifecycle.
type RunStatus string

const (
	RunStatusPending          RunStatus = "pending"
	RunStatusPlanning         RunStatus = "planning"
	RunStatusAwaitingApproval RunStatus = "awaiting_approval"
	RunStatusExecuting        RunStatus = "executing"
	RunStatusCompleted        RunStatus = "completed"
	RunStatusFailed           RunStatus = "failed"
	RunStatusCancelled        RunStatus = "cancelled"
)

// ApprovalStatus tracks a single proposed mutation.
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "pending"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusRejected ApprovalStatus = "rejected"
	ApprovalStatusExpired  ApprovalStatus = "expired"
)

// GroundingState records how much of the user's own notes reached the planner.
//
// Three states rather than a boolean, because "searched and found nothing
// relevant" and "could not search" have different causes and warrant different
// responses: the first means their notes do not cover the request, the second means
// retrieval is broken.
type GroundingState string

const (
	// GroundingGrounded means at least one retrieved note reached the prompt.
	GroundingGrounded GroundingState = "grounded"

	// GroundingNoContext means retrieval ran and matched nothing. The plan was
	// written without the user's notes because none were relevant.
	GroundingNoContext GroundingState = "no_context"

	// GroundingUngrounded means retrieval failed and the plan was written without
	// the user's notes for a reason that is not their fault.
	GroundingUngrounded GroundingState = "ungrounded"
)

// PlanIntent is what a plan is asking for, as opposed to how it will be carried
// out.
//
// The model may only choose from these values, and it never names a tool. That
// separation is the point: an intent is a statement of intent, while the mechanism
// is chosen in code by the agent's tool registry and confirmed by the user's
// approval. A model that could name a connector could reach an account it was never
// asked about.
type PlanIntent string

const (
	// PlanIntentTask records the request as an item of work.
	PlanIntentTask PlanIntent = "task"

	// PlanIntentCalendarEvent proposes creating a calendar event.
	PlanIntentCalendarEvent PlanIntent = "calendar_event"
)

// AllPlanIntents is the order used to build the planner's schema and to validate
// an intent.
var AllPlanIntents = []PlanIntent{PlanIntentTask, PlanIntentCalendarEvent}

// IsValid reports whether i is a known intent.
func (i PlanIntent) IsValid() bool {
	for _, known := range AllPlanIntents {
		if known == i {
			return true
		}
	}

	return false
}

// AgentRun is the durable record of one request to the orchestrator.
//
// Plan and Result are stored as JSONB because their shape belongs to the agent
// layer and will evolve; the lifecycle fields around them are the part worth
// querying, and those are real columns.
type AgentRun struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	Goal   string    `gorm:"type:text;not null" json:"goal"`
	Status RunStatus `gorm:"type:text;not null" json:"status"`
	// Plan is the validated step list the model produced.
	Plan json.RawMessage `gorm:"type:jsonb" json:"plan,omitempty"`
	// Result is whatever the executor produced.
	Result  json.RawMessage `gorm:"type:jsonb" json:"result,omitempty"`
	Error   string          `gorm:"type:text" json:"error,omitempty"`
	TraceID string          `gorm:"type:text" json:"trace_id,omitempty"`
	Model   string          `gorm:"type:text" json:"model,omitempty"`
	// PlanFallback is true when Plan is the deterministic fallback rather than a
	// model-produced plan. The fallback is the user's own goal turned into a title,
	// so it reads like a real plan and has to be labelled as one.
	PlanFallback bool `gorm:"not null;default:false" json:"plan_fallback"`
	// GroundingState says whether the plan was written with the user's notes in
	// front of the model. It is stored for the same reason PlanFallback is: an
	// ungrounded plan is otherwise indistinguishable from a grounded one, and a
	// user approving a plan has no way to know which they are looking at.
	GroundingState GroundingState `gorm:"type:text;not null;default:no_context" json:"grounding_state"`
	StartedAt      *time.Time     `gorm:"type:timestamptz" json:"started_at"`
	FinishedAt     *time.Time     `gorm:"type:timestamptz" json:"finished_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func (AgentRun) TableName() string { return "agent_runs" }

// allowedTransitions is the run lifecycle. Failure and cancellation are
// reachable from any live state because both an unhappy model and an impatient
// user can arrive at any point; everything else has to be earned in order.
var allowedTransitions = map[RunStatus][]RunStatus{
	RunStatusPending:          {RunStatusPlanning, RunStatusCancelled, RunStatusFailed},
	RunStatusPlanning:         {RunStatusAwaitingApproval, RunStatusCancelled, RunStatusFailed},
	RunStatusAwaitingApproval: {RunStatusExecuting, RunStatusCompleted, RunStatusCancelled, RunStatusFailed},
	// executing goes back to awaiting_approval as well as failing, because a tool call
	// can fail for a reason that has nothing to do with the decision the user made: a
	// dropped connection, an expired credential, a provider timeout. Failing the run
	// there strands the user, who approved something reasonable and now has to ask
	// again to find out whether it worked.
	RunStatusExecuting: {RunStatusAwaitingApproval, RunStatusCompleted, RunStatusFailed, RunStatusCancelled},
	RunStatusCompleted: {},
	RunStatusFailed:    {},
	RunStatusCancelled: {},
}

// Terminal reports whether the run has stopped and can no longer change.
func (r AgentRun) Terminal() bool {
	return r.Status == RunStatusCompleted || r.Status == RunStatusFailed || r.Status == RunStatusCancelled
}

func (r *AgentRun) canTransitionTo(to RunStatus) bool {
	for _, allowed := range allowedTransitions[r.Status] {
		if allowed == to {
			return true
		}
	}

	return false
}

// moveTo applies a status change, stamping the lifecycle timestamps that go with
// it. Every state change goes through here so an illegal one cannot reach the
// database, where the CHECK constraint would reject it as an opaque error.
func (r *AgentRun) moveTo(to RunStatus, now time.Time, reason string) error {
	if !r.canTransitionTo(to) {
		return ErrStateTransition
	}

	r.Status = to
	r.UpdatedAt = now

	if r.StartedAt == nil && to != RunStatusPending {
		r.StartedAt = &now
	}

	switch to {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled:
		r.FinishedAt = &now
	}

	if reason != "" {
		r.Error = reason
	}

	return nil
}

// StartPlanning marks the run as actively being planned.
func (r *AgentRun) StartPlanning(now time.Time) error {
	return r.moveTo(RunStatusPlanning, now, "")
}

// AwaitApproval parks the run until the user decides on its proposals.
func (r *AgentRun) AwaitApproval(now time.Time) error {
	return r.moveTo(RunStatusAwaitingApproval, now, "")
}

// BeginExecuting marks the run as running its approved steps.
func (r *AgentRun) BeginExecuting(now time.Time) error {
	return r.moveTo(RunStatusExecuting, now, "")
}

// Retry returns an executing run to awaiting approval after a tool call failed for a
// reason the user's decision did not cause.
//
// The recorded reason is kept so the next attempt, or the user, can see what went
// wrong. It is cleared on completion, because a run that eventually succeeds did not
// fail, and leaving the stale message behind makes a working run look broken.
func (r *AgentRun) Retry(now time.Time, reason string) error {
	if err := r.moveTo(RunStatusAwaitingApproval, now, reason); err != nil {
		return err
	}

	// The previous attempt is over, so it must not leave the run looking finished.
	r.FinishedAt = nil

	return nil
}

// Complete finishes a successful run.
func (r *AgentRun) Complete(now time.Time) error {
	// A run that failed an attempt and then succeeded has no failure to report, and a
	// leftover reason would be read as "this is broken" by anything showing it.
	r.Error = ""

	return r.moveTo(RunStatusCompleted, now, "")
}

// Fail stops the run and records why.
func (r *AgentRun) Fail(now time.Time, reason string) error {
	return r.moveTo(RunStatusFailed, now, reason)
}

// Cancel stops the run at the user's request.
func (r *AgentRun) Cancel(now time.Time) error {
	return r.moveTo(RunStatusCancelled, now, "")
}

// NewAgentRun starts a run in the pending state.
func NewAgentRun(userID uuid.UUID, goal string, now time.Time) (*AgentRun, error) {
	trimmed := strings.TrimSpace(goal)
	if trimmed == "" {
		return nil, Invalid("goal is required", map[string]string{"goal": "must not be empty"})
	}

	return &AgentRun{
		ID:        uuid.New(),
		UserID:    userID,
		Goal:      trimmed,
		Status:    RunStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// RunFilter narrows a run listing. An empty filter matches every run the caller
// owns.
type RunFilter struct {
	Status RunStatus
}

// AllRunStatuses is the order used to validate filter parameters.
var AllRunStatuses = []RunStatus{
	RunStatusPending,
	RunStatusPlanning,
	RunStatusAwaitingApproval,
	RunStatusExecuting,
	RunStatusCompleted,
	RunStatusFailed,
	RunStatusCancelled,
}

// IsValid reports whether s is a known run status.
func (s RunStatus) IsValid() bool {
	for _, known := range AllRunStatuses {
		if s == known {
			return true
		}
	}

	return false
}

// Approval is a proposed mutation awaiting the user's decision.
type Approval struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	RunID     uuid.UUID `gorm:"type:uuid;not null;index" json:"run_id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	StepIndex int       `gorm:"not null" json:"step_index"`
	ToolName  string    `gorm:"type:text;not null" json:"tool_name"`
	// Arguments is the exact payload the tool will receive, shown to the user
	// for approval. Storing it verbatim means what is reviewed is what runs.
	Arguments  json.RawMessage `gorm:"type:jsonb;not null" json:"arguments"`
	Rationale  string          `gorm:"type:text;not null" json:"rationale"`
	Status     ApprovalStatus  `gorm:"type:text;not null" json:"status"`
	ResolvedAt *time.Time      `gorm:"type:timestamptz" json:"resolved_at"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (Approval) TableName() string { return "approvals" }

// NewApproval records a pending proposal for one step of a run.
func NewApproval(runID uuid.UUID, userID uuid.UUID, stepIndex int, toolName string, arguments json.RawMessage, rationale string, now time.Time) (*Approval, error) {
	if len(arguments) == 0 {
		return nil, Invalid("tool arguments are required", nil)
	}

	if !json.Valid(arguments) {
		return nil, Invalid("tool arguments must be valid JSON", nil)
	}

	if strings.TrimSpace(toolName) == "" {
		return nil, Invalid("tool name is required", nil)
	}

	if stepIndex < 0 {
		return nil, Invalid("step index must not be negative", nil)
	}

	return &Approval{
		ID:        uuid.New(),
		RunID:     runID,
		UserID:    userID,
		StepIndex: stepIndex,
		ToolName:  strings.TrimSpace(toolName),
		Arguments: arguments,
		Rationale: strings.TrimSpace(rationale),
		Status:    ApprovalStatusPending,
		CreatedAt: now,
	}, nil
}

// Approve accepts the proposal. Only a pending approval can be decided, which
// stops a second click from re-running a mutation.
func (a *Approval) Approve(now time.Time) error {
	if a.Status != ApprovalStatusPending {
		return ErrStateTransition
	}

	a.Status = ApprovalStatusApproved
	a.ResolvedAt = &now

	return nil
}

// Release returns an approved approval to pending so it can be decided again.
//
// It exists for the case where the decision was fine and the execution was not: a
// dropped connection or an expired credential should not consume the user's approval
// permanently, forcing them to ask for the whole thing again to find out whether it
// worked.
//
// Only from approved. Releasing a rejected or expired approval would resurrect a
// decision the user closed off, and releasing a pending one is a no-op that would mask
// a double-click rather than report it.
func (a *Approval) Release(now time.Time) error {
	if a.Status != ApprovalStatusApproved {
		return ErrStateTransition
	}

	a.Status = ApprovalStatusPending
	a.ResolvedAt = nil

	return nil
}

// Reject declines the proposal.
func (a *Approval) Reject(now time.Time) error {
	if a.Status != ApprovalStatusPending {
		return ErrStateTransition
	}

	a.Status = ApprovalStatusRejected
	a.ResolvedAt = &now

	return nil
}
