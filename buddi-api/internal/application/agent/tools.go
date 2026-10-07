package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// taskCreateToolName is the stable identifier stored on every task approval.
const taskCreateToolName = "tasks.create"

// sourceAgentRun marks tasks the agent created, so a user can tell at a glance
// which of their tasks did not arrive by hand.
const sourceAgentRun = "agent"

// TaskCreator is the slice of the task service this tool needs. Depending on it
// rather than the concrete service keeps the tool testable without a repository.
type TaskCreator interface {
	Create(ctx context.Context, userID uuid.UUID, input task.CreateInput) (*domain.Task, error)
}

// TaskCreateTool creates one task from an approved plan.
//
// It is an in-process implementation of the Tool contract. An MCP-backed tool
// with the same name and argument shape can replace it later without the
// orchestration above it noticing.
type TaskCreateTool struct {
	tasks TaskCreator
}

// NewTaskCreateTool builds the tool.
func NewTaskCreateTool(tasks TaskCreator) *TaskCreateTool {
	return &TaskCreateTool{tasks: tasks}
}

// taskCreatePayload is the exact payload shown to the user for approval.
type taskCreatePayload struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	DueAt       string `json:"due_at,omitempty"`
}

// Name implements Tool.
func (t *TaskCreateTool) Name() string { return taskCreateToolName }

// Description implements Tool.
func (t *TaskCreateTool) Description() string {
	return "Create a task in the user's task list."
}

// Mutating implements Tool. Creating a task changes the user's list, so it is a
// mutation and a plan may resolve to it.
func (t *TaskCreateTool) Mutating() bool { return true }

// Validate implements Tool. It runs before the proposal is shown, so the user is
// never asked to approve arguments that would be refused on execution.
func (t *TaskCreateTool) Validate(arguments json.RawMessage) error {
	parsed, err := decodeTaskPayload(arguments)
	if err != nil {
		return err
	}

	if strings.TrimSpace(parsed.Title) == "" {
		return fmt.Errorf("title is required")
	}

	if len(parsed.Title) > domain.MaxTaskTitle {
		return fmt.Errorf("title must be at most %d characters", domain.MaxTaskTitle)
	}

	if len(parsed.Description) > domain.MaxTaskDescription {
		return fmt.Errorf("description must be at most %d characters", domain.MaxTaskDescription)
	}

	if parsed.Priority != "" && !domain.TaskPriority(parsed.Priority).IsValid() {
		return fmt.Errorf("priority must be one of low, normal, high")
	}

	if parsed.DueAt != "" {
		if _, err := parseRFC3339(parsed.DueAt); err != nil {
			return fmt.Errorf("due_at must be an RFC 3339 timestamp")
		}
	}

	return nil
}

// Execute implements Tool.
func (t *TaskCreateTool) Execute(ctx context.Context, userID uuid.UUID, runID uuid.UUID, arguments json.RawMessage) (json.RawMessage, error) {
	parsed, err := decodeTaskPayload(arguments)
	if err != nil {
		return nil, err
	}

	var dueAt *time.Time
	if parsed.DueAt != "" {
		parsedDue, err := parseRFC3339(parsed.DueAt)
		if err != nil {
			return nil, err
		}

		dueAt = parsedDue
	}

	priority := domain.TaskPriority(parsed.Priority)
	if priority == "" {
		priority = domain.TaskPriorityNormal
	}

	created, err := t.tasks.Create(ctx, userID, task.CreateInput{
		Title:       parsed.Title,
		Description: parsed.Description,
		Priority:    priority,
		DueAt:       dueAt,
		Source:      sourceAgentRun,
		RunID:       &runID,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"task_id": created.ID.String(),
		"title":   created.Title,
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("could not encode tool result: %w", err)
	}

	return encoded, nil
}

// EncodeTaskCreateArguments renders a plan as tasks.create arguments.
//
// The plan's steps become a checklist in the description rather than separate
// tasks, which keeps one plan to one proposal.
//
// Exported because the payload is a contract: a connector registering alongside this
// tool needs to know what the task tool considers a complete plan, and the tests
// that assert the approval screen do not get to use a stub instead.
func EncodeTaskCreateArguments(plan *planner.Plan) (json.RawMessage, error) {
	arguments := taskCreatePayload{
		Title:       plan.Title,
		Description: describeSteps(plan),
		Priority:    string(plan.Priority),
	}

	if plan.DueAt != nil {
		arguments.DueAt = plan.DueAt.UTC().Format(time.RFC3339)
	}

	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("could not encode tool arguments: %w", err)
	}

	return encoded, nil
}

// TaskCreateBinding is the registry entry for task creation.
//
// It is the default: a plan whose intent this registry does not recognise becomes a
// task, because recording something locally is recoverable and a misrouted
// connector call is not.
func TaskCreateBinding(tasks TaskCreator) RegistryOption {
	return RegistryOption{
		Intent:    domain.PlanIntentTask,
		Tool:      NewTaskCreateTool(tasks),
		Encode:    EncodeTaskCreateArguments,
		Rationale: taskCreateRationale,
		Default:   true,
	}
}

// taskCreateRationale explains the proposal on the approval screen, in the user's
// terms rather than the tool's.
func taskCreateRationale(plan *planner.Plan) string {
	if len(plan.Steps) == 0 {
		return "Creates one task from the plan."
	}

	steps := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		steps = append(steps, step.Description)
	}

	return "Creates one task with these steps: " + strings.Join(steps, "; ")
}

func describeSteps(plan *planner.Plan) string {
	var builder strings.Builder

	if plan.Description != "" {
		builder.WriteString(plan.Description)
	}

	for i, step := range plan.Steps {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}

		fmt.Fprintf(&builder, "%d. %s", i+1, step.Description)
	}

	return builder.String()
}

// parseRFC3339 parses a timestamp that arrived as a JSON string. It exists so
// validation and execution cannot disagree about what is acceptable.
func parseRFC3339(raw string) (*time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

// decodeTaskPayload rejects unknown fields so a proposal cannot smuggle in
// data the tool would silently ignore.
func decodeTaskPayload(raw json.RawMessage) (taskCreatePayload, error) {
	var parsed taskCreatePayload

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&parsed); err != nil {
		return taskCreatePayload{}, fmt.Errorf("arguments are not valid tool input: %w", err)
	}

	return parsed, nil
}
