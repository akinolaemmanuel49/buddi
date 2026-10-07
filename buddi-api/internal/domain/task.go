package domain

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxTaskTitle       = 300
	MaxTaskDescription = 5000
)

// TaskStatus tracks where a task is in its lifecycle.
type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusCompleted  TaskStatus = "completed"
	TaskStatusCancelled  TaskStatus = "cancelled"
)

// AllTaskStatuses is the order used to validate filter parameters.
var AllTaskStatuses = []TaskStatus{
	TaskStatusPending,
	TaskStatusInProgress,
	TaskStatusCompleted,
	TaskStatusCancelled,
}

// IsValid reports whether s is a known status.
func (s TaskStatus) IsValid() bool {
	for _, known := range AllTaskStatuses {
		if s == known {
			return true
		}
	}

	return false
}

// Terminal reports whether the status can still change.
func (s TaskStatus) Terminal() bool {
	return s == TaskStatusCompleted || s == TaskStatusCancelled
}

// TaskPriority orders work within a status.
type TaskPriority string

const (
	TaskPriorityLow    TaskPriority = "low"
	TaskPriorityNormal TaskPriority = "normal"
	TaskPriorityHigh   TaskPriority = "high"
)

var AllTaskPriorities = []TaskPriority{
	TaskPriorityLow,
	TaskPriorityNormal,
	TaskPriorityHigh,
}

// IsValid reports whether p is a known priority.
func (p TaskPriority) IsValid() bool {
	for _, known := range AllTaskPriorities {
		if p == known {
			return true
		}
	}

	return false
}

// Task is something the user needs to do.
type Task struct {
	ID          uuid.UUID    `gorm:"type:uuid;primaryKey" json:"id"`
	UserID      uuid.UUID    `gorm:"type:uuid;not null;index" json:"user_id"`
	Title       string       `gorm:"type:text;not null" json:"title"`
	Description string       `gorm:"type:text;not null" json:"description"`
	Status      TaskStatus   `gorm:"type:text;not null" json:"status"`
	Priority    TaskPriority `gorm:"type:text;not null" json:"priority"`
	DueAt       *time.Time   `gorm:"type:timestamptz" json:"due_at"`
	CompletedAt *time.Time   `gorm:"type:timestamptz" json:"completed_at"`
	// Source records whether the task came from the user or from an agent run.
	Source    string     `gorm:"type:text;not null" json:"source"`
	RunID     *uuid.UUID `gorm:"type:uuid" json:"run_id"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (Task) TableName() string { return "tasks" }

// NewTask validates and normalises a task ready to be stored.
func NewTask(userID uuid.UUID, title string, description string, priority TaskPriority, dueAt *time.Time, source string, now time.Time) (*Task, error) {
	trimmedTitle := strings.TrimSpace(title)
	if trimmedTitle == "" {
		return nil, Invalid("title is required", map[string]string{"title": "must not be empty"})
	}

	if utf8.RuneCountInString(trimmedTitle) > MaxTaskTitle {
		return nil, Invalid("title is too long", map[string]string{
			"title": "must be at most " + strconv.Itoa(MaxTaskTitle) + " characters",
		})
	}

	if utf8.RuneCountInString(description) > MaxTaskDescription {
		return nil, Invalid("description is too long", map[string]string{
			"description": "must be at most " + strconv.Itoa(MaxTaskDescription) + " characters",
		})
	}

	if priority == "" {
		priority = TaskPriorityNormal
	}

	if !priority.IsValid() {
		return nil, Invalid("priority is not valid", map[string]string{
			"priority": "must be one of low, normal, high",
		})
	}

	if source == "" {
		source = NoteSourceManual
	}

	return &Task{
		ID:          uuid.New(),
		UserID:      userID,
		Title:       trimmedTitle,
		Description: description,
		Status:      TaskStatusPending,
		Priority:    priority,
		DueAt:       dueAt,
		Source:      source,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Complete marks the task done. Completing an already completed task succeeds
// and leaves the original completion time alone, which makes the operation safe
// to retry from an agent.
func (t *Task) Complete(now time.Time) error {
	if t.Status == TaskStatusCancelled {
		return ErrStateTransition
	}

	if t.Status == TaskStatusCompleted {
		return nil
	}

	t.Status = TaskStatusCompleted
	t.CompletedAt = &now
	t.UpdatedAt = now

	return nil
}

// Cancel moves a task out of the workflow. A completed task cannot be cancelled
// because that would erase the fact that the work was done.
func (t *Task) Cancel(now time.Time) error {
	if t.Status == TaskStatusCompleted {
		return ErrStateTransition
	}

	if t.Status == TaskStatusCancelled {
		return nil
	}

	t.Status = TaskStatusCancelled
	t.CompletedAt = nil
	t.UpdatedAt = now

	return nil
}

// ChangeStatus applies a new status, keeping CompletedAt consistent.
func (t *Task) ChangeStatus(next TaskStatus, now time.Time) error {
	if !next.IsValid() {
		return Invalid("status is not valid", map[string]string{
			"status": "must be one of pending, in_progress, completed, cancelled",
		})
	}

	switch next {
	case TaskStatusCompleted:
		return t.Complete(now)
	case TaskStatusCancelled:
		return t.Cancel(now)
	default:
		if t.Status.Terminal() {
			return ErrStateTransition
		}

		t.Status = next
		t.CompletedAt = nil
		t.UpdatedAt = now

		return nil
	}
}
