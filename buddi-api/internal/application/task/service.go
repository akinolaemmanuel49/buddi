// Package task implements the task use cases, including the state machine that
// decides whether a task may be completed or cancelled.
package task

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// MaxPageSize bounds a single page of results.
const MaxPageSize = 100

// Service holds the dependencies of the task use cases.
type Service struct {
	tasks domain.TaskRepository
	now   func() time.Time
}

// NewService builds the service. now may be nil, in which case time.Now is used.
func NewService(tasks domain.TaskRepository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}

	return &Service{tasks: tasks, now: now}
}

// CreateInput carries the fields of a new task.
type CreateInput struct {
	Title       string
	Description string
	Priority    domain.TaskPriority
	DueAt       *time.Time
	Source      string
	RunID       *uuid.UUID
}

// Create validates and stores a new task.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, input CreateInput) (*domain.Task, error) {
	entity, err := domain.NewTask(userID, input.Title, input.Description, input.Priority, input.DueAt, input.Source, s.now())
	if err != nil {
		return nil, err
	}

	entity.RunID = input.RunID

	if err := s.tasks.Create(ctx, entity); err != nil {
		return nil, err
	}

	return entity, nil
}

// UpdateInput carries a partial update. Nil fields are left alone.
type UpdateInput struct {
	Title       *string
	Description *string
	Priority    *domain.TaskPriority
	DueAt       *time.Time
	// ClearDueAt removes the due date, which a nil DueAt alone cannot express.
	ClearDueAt bool
}

// Update applies a partial update to a task owned by userID.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, taskID uuid.UUID, input UpdateInput) (*domain.Task, error) {
	existing, err := s.tasks.GetByID(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}

	if input.Title != nil {
		trimmed := strings.TrimSpace(*input.Title)
		if trimmed == "" {
			return nil, domain.Invalid("title is required", map[string]string{"title": "must not be empty"})
		}

		existing.Title = trimmed
	}

	if input.Description != nil {
		existing.Description = *input.Description
	}

	if input.Priority != nil {
		if !input.Priority.IsValid() {
			return nil, domain.Invalid("priority is not valid", map[string]string{
				"priority": "must be one of low, normal, high",
			})
		}

		existing.Priority = *input.Priority
	}

	if input.ClearDueAt {
		existing.DueAt = nil
	} else if input.DueAt != nil {
		dueAt := *input.DueAt
		existing.DueAt = &dueAt
	}

	existing.UpdatedAt = s.now()

	if err := s.tasks.Update(ctx, existing); err != nil {
		return nil, err
	}

	return existing, nil
}

// SetStatus moves a task to a new status, enforcing the state machine.
func (s *Service) SetStatus(ctx context.Context, userID uuid.UUID, taskID uuid.UUID, status domain.TaskStatus) (*domain.Task, error) {
	existing, err := s.tasks.GetByID(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}

	if err := existing.ChangeStatus(status, s.now()); err != nil {
		return nil, err
	}

	if err := s.tasks.Update(ctx, existing); err != nil {
		return nil, err
	}

	return existing, nil
}

// Complete marks a task done. Completing an already completed task succeeds, so
// an agent that retries after a timeout does not fail.
func (s *Service) Complete(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error) {
	return s.SetStatus(ctx, userID, taskID, domain.TaskStatusCompleted)
}

// List returns a page of the user's tasks.
func (s *Service) List(ctx context.Context, userID uuid.UUID, filter domain.TaskFilter, page domain.Page) ([]domain.Task, int64, error) {
	page = page.Normalise(MaxPageSize)

	tasks, err := s.tasks.List(ctx, userID, filter, page)
	if err != nil {
		return nil, 0, err
	}

	total, err := s.tasks.Count(ctx, userID, filter)
	if err != nil {
		return nil, 0, err
	}

	return tasks, total, nil
}

// Get returns one task owned by userID.
func (s *Service) Get(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) (*domain.Task, error) {
	return s.tasks.GetByID(ctx, userID, taskID)
}

// Delete removes a task owned by userID.
func (s *Service) Delete(ctx context.Context, userID uuid.UUID, taskID uuid.UUID) error {
	return s.tasks.Delete(ctx, userID, taskID)
}
