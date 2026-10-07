package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/oauth"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// plannedRunWithApproval plans a goal and returns a service sharing the store, the
// owner, and the ids the test needs to act on it.
func plannedRunWithApproval(
	t *testing.T,
	store *fakeStore,
	tool agent.Tool,
) (svc *agent.Service, owner uuid.UUID, approvalID uuid.UUID, runID uuid.UUID) {
	t.Helper()

	owner = uuid.New()

	planned, err := newService(t, store, &fakePlanner{outcome: goodPlan()}, tool).
		Plan(context.Background(), owner, "buy milk")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// A service built over the same store, so the test approves against the run the
	// first service created.
	svc = newService(t, store, &fakePlanner{outcome: goodPlan()}, tool)

	return svc, owner, planned.Approvals[0].ID, planned.Run.ID
}

func TestIsTransientToolError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil is not an error": {nil, false},
		// The call demonstrably never completed, so the same decision can be re-made.
		"timed out":        {context.DeadlineExceeded, true},
		"cancelled":        {context.Canceled, true},
		"deadline wrapped": {errors.Join(errors.New("mcp: call failed"), context.DeadlineExceeded), true},
		// A credential the caller must renew is not the user's decision.
		"revoked credential": {oauth.ErrRevoked, true},
		"not connected":      {oauth.ErrNotConnected, true},
		// The provider understood the request and declined it. Retrying repeats the
		// refusal, and if it half-succeeded it writes twice.
		"invalid argument": {errors.New("calendar: end must be after start"), false},
		"not found":        {errors.New("calendar 404"), false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := agent.IsTransientToolError(tc.err); got != tc.want {
				t.Errorf("IsTransientToolError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The failure this exists for: the tool call fails for a reason the user's decision did
// not cause, the approval is consumed, and the user has to ask for the whole thing again.
func TestARetriedExecutionLeavesTheApprovalDecidable(t *testing.T) {
	store := newFakeStore()
	tool := &fakeTool{name: "tasks.create", executeErr: context.DeadlineExceeded}

	service, owner, approvalID, _ := plannedRunWithApproval(t, store, tool)

	run, err := service.Approve(context.Background(), owner, approvalID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Not an error: nothing was lost and the decision still stands.
	if run.Run.Status != domain.RunStatusAwaitingApproval {
		t.Errorf("run status = %q, want %q so the decision can be made again",
			run.Run.Status, domain.RunStatusAwaitingApproval)
	}

	if len(run.Approvals) != 1 || run.Approvals[0].Status != domain.ApprovalStatusPending {
		t.Errorf("approval = %+v, want it back to pending", run.Approvals)
	}

	if run.Run.Error == "" {
		t.Error("the reason was not recorded, so the UI cannot explain itself")
	}

	if run.Run.FinishedAt != nil {
		t.Error("FinishedAt is set on a run that is still awaiting a decision")
	}

	// The point of all of it: the same approval can now be made again.
	if _, err := service.Approve(context.Background(), owner, approvalID); err != nil {
		t.Errorf("second Approve: %v", err)
	}
}

func TestARetryingRunClearsItsReasonWhenItSucceeds(t *testing.T) {
	store := newFakeStore()
	tool := &fakeTool{name: "tasks.create", executeErr: context.DeadlineExceeded}

	service, owner, approvalID, _ := plannedRunWithApproval(t, store, tool)

	run, err := service.Approve(context.Background(), owner, approvalID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if run.Run.Error == "" {
		t.Fatal("no reason recorded on the first attempt")
	}

	tool.executeErr = nil

	done, err := service.Approve(context.Background(), owner, approvalID)
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}

	if done.Run.Status != domain.RunStatusCompleted {
		t.Errorf("status = %q, want completed", done.Run.Status)
	}

	// A run that eventually succeeded has no failure to report, and a leftover reason
	// reads as "this is broken".
	if done.Run.Error != "" {
		t.Errorf("Error = %q on a completed run, want it cleared", done.Run.Error)
	}
}

// A refusal is not transient, so the approval stays consumed and the run fails. This
// is the half of the rule that stops a refusal becoming an infinite retry.
func TestANonTransientFailureStillConsumesTheApproval(t *testing.T) {
	store := newFakeStore()
	tool := &fakeTool{name: "tasks.create", executeErr: errors.New("calendar: not permitted")}

	service, owner, approvalID, runID := plannedRunWithApproval(t, store, tool)

	if _, err := service.Approve(context.Background(), owner, approvalID); err == nil {
		t.Fatal("Approve hid a tool failure")
	}

	stored, err := store.GetApproval(context.Background(), owner, approvalID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}

	if stored.Status != domain.ApprovalStatusApproved {
		t.Errorf("approval = %q, want it consumed", stored.Status)
	}

	run, err := store.GetRun(context.Background(), owner, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if run.Status != domain.RunStatusFailed {
		t.Errorf("run status = %q, want failed", run.Status)
	}
}

// Releasing is only meaningful from approved. Releasing a decision the user closed off
// would resurrect it.
func TestApprovalReleaseOnlyAppliesToApproved(t *testing.T) {
	now := time.Now()

	pending := &domain.Approval{Status: domain.ApprovalStatusPending}
	if err := pending.Release(now); err == nil {
		t.Error("a pending approval was released")
	}

	rejected := &domain.Approval{Status: domain.ApprovalStatusRejected}
	if err := rejected.Release(now); err == nil {
		t.Error("a rejected approval was released")
	}

	approved := &domain.Approval{Status: domain.ApprovalStatusApproved, ResolvedAt: &now}
	if err := approved.Release(now); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if approved.Status != domain.ApprovalStatusPending {
		t.Errorf("status = %q, want pending", approved.Status)
	}

	if approved.ResolvedAt != nil {
		t.Error("ResolvedAt survived the release, so it still looks decided")
	}
}

// The transition has to exist for a run to come back, and only from executing: a
// terminal run must stay terminal or a failed one could be resurrected.
func TestRunRetryIsOnlyReachableFromExecuting(t *testing.T) {
	now := time.Now()

	run := &domain.AgentRun{Status: domain.RunStatusExecuting}
	if err := run.Retry(now, "tool failed"); err != nil {
		t.Fatalf("Retry from executing: %v", err)
	}

	if run.Status != domain.RunStatusAwaitingApproval {
		t.Errorf("status = %q, want awaiting_approval", run.Status)
	}

	if run.Error != "tool failed" {
		t.Errorf("Error = %q, want the reason recorded", run.Error)
	}

	for _, status := range []domain.RunStatus{
		domain.RunStatusFailed,
		domain.RunStatusCompleted,
		domain.RunStatusCancelled,
		domain.RunStatusAwaitingApproval,
	} {
		other := &domain.AgentRun{Status: status}
		if err := other.Retry(now, "again"); err == nil {
			t.Errorf("Retry succeeded from %q, want a refused transition", status)
		}
	}
}
