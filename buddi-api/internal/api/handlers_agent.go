package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

type createRunRequest struct {
	Goal string `json:"goal"`
}

// approvalResponse shows the user exactly what would run. Arguments are passed
// through as raw JSON rather than decoded into a struct so the bytes that were
// validated are the bytes that are displayed.
type approvalResponse struct {
	ID        string          `json:"id"`
	StepIndex int             `json:"step_index"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Rationale string          `json:"rationale"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
}

type runResponse struct {
	ID        string             `json:"id"`
	Goal      string             `json:"goal"`
	Status    string             `json:"status"`
	Plan      json.RawMessage    `json:"plan,omitempty"`
	Approvals []approvalResponse `json:"approvals"`
	Result    json.RawMessage    `json:"result,omitempty"`
	Error     string             `json:"error,omitempty"`
	Model     string             `json:"model,omitempty"`
	TraceID   string             `json:"trace_id,omitempty"`
	// PlanFallback is always serialised, including when false. A client showing
	// proposals has to be able to tell a model plan from the deterministic
	// fallback, and an absent field is indistinguishable from an older payload.
	PlanFallback bool `json:"plan_fallback"`

	// GroundingState is always serialised for the same reason. It is the difference
	// between "this plan is based on your notes" and "the model decided on its own",
	// which is exactly the distinction a user cannot check any other way, and a field
	// that is only present when it is interesting cannot carry it.
	GroundingState string `json:"grounding_state"`

	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

type runListResponse struct {
	Data   []runResponse `json:"data"`
	Total  int64         `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

func toApprovalResponse(a domain.Approval) approvalResponse {
	return approvalResponse{
		ID:        a.ID.String(),
		StepIndex: a.StepIndex,
		ToolName:  a.ToolName,
		Arguments: a.Arguments,
		Rationale: a.Rationale,
		Status:    string(a.Status),
		CreatedAt: a.CreatedAt.UTC().Format(timeLayout),
	}
}

// groundingState keeps the field populated even for a run whose state was never set,
// because a client asking "was this grounded?" needs an answer in every case.
func groundingState(state domain.GroundingState) string {
	if state == "" {
		return string(domain.GroundingNoContext)
	}

	return string(state)
}

func toRunResponse(run *agent.Run) runResponse {
	response := runResponse{
		ID:           run.Run.ID.String(),
		Goal:         run.Run.Goal,
		Status:       string(run.Run.Status),
		Plan:         run.Run.Plan,
		Result:       run.Run.Result,
		Error:        run.Run.Error,
		Model:        run.Run.Model,
		TraceID:      run.Run.TraceID,
		PlanFallback: run.Run.PlanFallback,
		// An empty state is normalised rather than passed through: a run that predates
		// the column reads as no_context, and "the user's notes reached the planner" is
		// not a claim this layer is entitled to invent.
		GroundingState: groundingState(run.Run.GroundingState),
		CreatedAt:      run.Run.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt:      run.Run.UpdatedAt.UTC().Format(timeLayout),

		// Never nil: a client listing runs should not have to handle a missing
		// array to render "no proposals".
		Approvals: make([]approvalResponse, 0, len(run.Approvals)),
	}

	for i := range run.Approvals {
		response.Approvals = append(response.Approvals, toApprovalResponse(run.Approvals[i]))
	}

	if run.Run.StartedAt != nil {
		formatted := run.Run.StartedAt.UTC().Format(timeLayout)
		response.StartedAt = &formatted
	}

	if run.Run.FinishedAt != nil {
		formatted := run.Run.FinishedAt.UTC().Format(timeLayout)
		response.FinishedAt = &formatted
	}

	return response
}

// handleCreateRun plans a goal and returns the run awaiting approval.
//
// It blocks for the length of the model call, which on the CPU runtime is around
// ten seconds. That is a deliberate trade: the alternative is a worker, an orphan
// sweeper and a polling client, none of which are worth their complexity before
// there is more than one tool.
func (a *API) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	var body createRunRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	created, err := a.agent.Plan(r.Context(), userID, body.Goal)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusCreated, toRunResponse(created))
}

func (a *API) handleListRuns(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	page, err := Pagination(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	filter, err := runFilterFromRequest(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	list, err := a.agent.List(r.Context(), userID, filter, domain.Page{
		Limit:  page.Limit,
		Offset: page.Offset,
		Order:  page.Order,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	data := make([]runResponse, 0, len(list.Runs))
	for i := range list.Runs {
		run := &agent.Run{Run: &list.Runs[i]}

		data = append(data, toRunResponse(run))
	}

	WriteJSON(w, r, http.StatusOK, runListResponse{
		Data:   data,
		Total:  list.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

// runFilterFromRequest rejects unknown statuses up front, so a typo returns an
// error instead of an empty list that reads as "no runs".
func runFilterFromRequest(r *http.Request) (domain.RunFilter, error) {
	var filter domain.RunFilter

	if raw := strings.TrimSpace(Query(r, "status")); raw != "" {
		status := domain.RunStatus(raw)
		if !status.IsValid() {
			return domain.RunFilter{}, BadRequest("status must be a known run status")
		}

		filter.Status = status
	}

	return filter, nil
}

func (a *API) handleGetRun(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	runID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	found, err := a.agent.Get(r.Context(), userID, runID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toRunResponse(found))
}

func (a *API) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	runID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	cancelled, err := a.agent.Cancel(r.Context(), userID, runID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toRunResponse(cancelled))
}

func (a *API) handleApprove(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	approvalID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	approved, err := a.agent.Approve(r.Context(), userID, approvalID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toRunResponse(approved))
}

func (a *API) handleReject(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	approvalID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	rejected, err := a.agent.Reject(r.Context(), userID, approvalID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toRunResponse(rejected))
}
