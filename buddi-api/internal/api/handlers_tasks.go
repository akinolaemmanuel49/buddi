package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

type createTaskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	DueAt       string `json:"due_at"`
}

type updateTaskRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Priority    *string `json:"priority"`
	DueAt       *string `json:"due_at"`
	ClearDueAt  bool    `json:"clear_due_at"`
	Status      *string `json:"status"`
}

type taskResponse struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	DueAt       *string `json:"due_at"`
	CompletedAt *string `json:"completed_at"`
	Source      string  `json:"source"`
	RunID       *string `json:"run_id"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type taskListResponse struct {
	Data   []taskResponse `json:"data"`
	Total  int64          `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func toTaskResponse(t *domain.Task) taskResponse {
	response := taskResponse{
		ID:          t.ID.String(),
		Title:       t.Title,
		Description: t.Description,
		Status:      string(t.Status),
		Priority:    string(t.Priority),
		Source:      t.Source,
		CreatedAt:   t.CreatedAt.UTC().Format(timeLayout),
		UpdatedAt:   t.UpdatedAt.UTC().Format(timeLayout),
	}

	if t.DueAt != nil {
		formatted := t.DueAt.UTC().Format(timeLayout)
		response.DueAt = &formatted
	}

	if t.CompletedAt != nil {
		formatted := t.CompletedAt.UTC().Format(timeLayout)
		response.CompletedAt = &formatted
	}

	if t.RunID != nil {
		id := t.RunID.String()
		response.RunID = &id
	}

	return response
}

func (a *API) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	var body createTaskRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	dueAt, err := parseOptionalTime(body.DueAt)
	if err != nil {
		WriteError(w, r, BadRequest("due_at must be an RFC 3339 timestamp"))

		return
	}

	created, err := a.tasks.Create(r.Context(), userID, task.CreateInput{
		Title:       body.Title,
		Description: body.Description,
		Priority:    domain.TaskPriority(body.Priority),
		DueAt:       dueAt,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusCreated, toTaskResponse(created))
}

func (a *API) handleListTasks(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	page, err := Pagination(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	filter, err := taskFilterFromRequest(r)
	if err != nil {
		WriteError(w, r, err)

		return
	}

	tasks, total, err := a.tasks.List(r.Context(), userID, filter, domain.Page{
		Limit:  page.Limit,
		Offset: page.Offset,
		Order:  page.Order,
	})
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	data := make([]taskResponse, 0, len(tasks))
	for i := range tasks {
		data = append(data, toTaskResponse(&tasks[i]))
	}

	WriteJSON(w, r, http.StatusOK, taskListResponse{
		Data:   data,
		Total:  total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

// taskFilterFromRequest reads list filters, rejecting unknown status or
// priority values up front rather than returning an empty page that looks like
// a successful search with no results.
func taskFilterFromRequest(r *http.Request) (domain.TaskFilter, error) {
	var filter domain.TaskFilter

	if raw := strings.TrimSpace(Query(r, "status")); raw != "" {
		status := domain.TaskStatus(raw)
		if !status.IsValid() {
			return domain.TaskFilter{}, BadRequest("status must be one of pending, in_progress, completed, cancelled")
		}

		filter.Status = status
	}

	if raw := strings.TrimSpace(Query(r, "priority")); raw != "" {
		priority := domain.TaskPriority(raw)
		if !priority.IsValid() {
			return domain.TaskFilter{}, BadRequest("priority must be one of low, normal, high")
		}

		filter.Priority = priority
	}

	dueBefore, err := parseOptionalTime(Query(r, "due_before"))
	if err != nil {
		return domain.TaskFilter{}, BadRequest("due_before must be an RFC 3339 timestamp")
	}

	filter.DueBefore = dueBefore

	dueAfter, err := parseOptionalTime(Query(r, "due_after"))
	if err != nil {
		return domain.TaskFilter{}, BadRequest("due_after must be an RFC 3339 timestamp")
	}

	filter.DueAfter = dueAfter

	if raw := Query(r, "run_id"); raw != "" {
		runID, err := parseUUID(raw)
		if err != nil {
			return domain.TaskFilter{}, BadRequest("run_id must be a valid identifier")
		}

		filter.RunID = &runID
	}

	return filter, nil
}

func (a *API) handleGetTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	taskID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	found, err := a.tasks.Get(r.Context(), userID, taskID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toTaskResponse(found))
}

func (a *API) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	taskID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	var body updateTaskRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)

		return
	}

	input := task.UpdateInput{ClearDueAt: body.ClearDueAt}

	if body.Title != nil {
		input.Title = body.Title
	}

	if body.Description != nil {
		input.Description = body.Description
	}

	if body.Priority != nil {
		priority := domain.TaskPriority(*body.Priority)
		input.Priority = &priority
	}

	if body.DueAt != nil {
		dueAt, err := parseOptionalTime(*body.DueAt)
		if err != nil {
			WriteError(w, r, BadRequest("due_at must be an RFC 3339 timestamp"))

			return
		}

		input.DueAt = dueAt
	}

	// A status change goes through the state machine, so it is applied after
	// the field updates rather than as part of them.
	if body.Status != nil {
		status := domain.TaskStatus(*body.Status)

		updated, err := a.tasks.Update(r.Context(), userID, taskID, input)
		if err != nil {
			a.writeServiceError(w, r, err)

			return
		}

		updated, err = a.tasks.SetStatus(r.Context(), userID, updated.ID, status)
		if err != nil {
			a.writeServiceError(w, r, err)

			return
		}

		WriteJSON(w, r, http.StatusOK, toTaskResponse(updated))

		return
	}

	updated, err := a.tasks.Update(r.Context(), userID, taskID, input)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toTaskResponse(updated))
}

func (a *API) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	taskID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	updated, err := a.tasks.Complete(r.Context(), userID, taskID)
	if err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	WriteJSON(w, r, http.StatusOK, toTaskResponse(updated))
}

func (a *API) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}

	taskID, ok := parseID(w, r, "id")
	if !ok {
		return
	}

	if err := a.tasks.Delete(r.Context(), userID, taskID); err != nil {
		a.writeServiceError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseOptionalTime accepts an empty string as "absent" and otherwise requires
// RFC 3339. An explicit "null" is not a valid timestamp string, but a JSON null
// unmarshals to the empty string, which this treats as absent.
func parseOptionalTime(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}
