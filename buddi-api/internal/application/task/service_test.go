package task_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

type fakeTasks struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]*domain.Task
	added []uuid.UUID
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{byID: map[uuid.UUID]*domain.Task{}}
}

func (f *fakeTasks) Create(_ context.Context, tk *domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored := *tk
	f.byID[tk.ID] = &stored
	f.added = append(f.added, tk.ID)

	return nil
}

func (f *fakeTasks) GetByID(_ context.Context, userID uuid.UUID, id uuid.UUID) (*domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored, ok := f.byID[id]
	if !ok || stored.UserID != userID {
		return nil, domain.ErrNotFound
	}

	copied := *stored

	return &copied, nil
}

func (f *fakeTasks) List(_ context.Context, userID uuid.UUID, filter domain.TaskFilter, page domain.Page) ([]domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := []domain.Task{}

	for _, stored := range f.byID {
		if stored.UserID != userID {
			continue
		}

		if filter.Status != "" && stored.Status != filter.Status {
			continue
		}

		if filter.Priority != "" && stored.Priority != filter.Priority {
			continue
		}

		out = append(out, *stored)
	}

	return out, nil
}

func (f *fakeTasks) Count(_ context.Context, userID uuid.UUID, _ domain.TaskFilter) (int64, error) {
	tasks, err := f.List(context.Background(), userID, domain.TaskFilter{}, domain.Page{})
	if err != nil {
		return 0, err
	}

	return int64(len(tasks)), nil
}

func (f *fakeTasks) Update(_ context.Context, tk *domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.byID[tk.ID]; !ok {
		return domain.ErrNotFound
	}

	stored := *tk
	f.byID[tk.ID] = &stored

	return nil
}

func (f *fakeTasks) Delete(_ context.Context, userID uuid.UUID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored, ok := f.byID[id]
	if !ok || stored.UserID != userID {
		return domain.ErrNotFound
	}

	delete(f.byID, id)

	return nil
}

type fixture struct {
	service *task.Service
	repo    *fakeTasks
	userID  uuid.UUID
	clock   *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := &start
	repo := newFakeTasks()

	return &fixture{
		service: task.NewService(repo, func() time.Time { return *clock }),
		repo:    repo,
		userID:  uuid.New(),
		clock:   clock,
	}
}

func (f *fixture) create(t *testing.T, title string) *domain.Task {
	t.Helper()

	created, err := f.service.Create(context.Background(), f.userID, task.CreateInput{
		Title:    title,
		Priority: domain.TaskPriorityNormal,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return created
}

func TestCreateAppliesDefaults(t *testing.T) {
	f := newFixture(t)

	created, err := f.service.Create(context.Background(), f.userID, task.CreateInput{Title: "  Write docs  "})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.Title != "Write docs" {
		t.Errorf("Title = %q, want it trimmed", created.Title)
	}

	if created.Status != domain.TaskStatusPending {
		t.Errorf("Status = %q, want pending", created.Status)
	}

	if created.Priority != domain.TaskPriorityNormal {
		t.Errorf("Priority = %q, want normal", created.Priority)
	}

	if created.CompletedAt != nil {
		t.Error("a new task must not have a completion time")
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name  string
		input task.CreateInput
	}{
		{name: "blank title", input: task.CreateInput{Title: "   "}},
		{name: "unknown priority", input: task.CreateInput{Title: "ok", Priority: "urgent"}},
		{name: "title too long", input: task.CreateInput{Title: string(make([]byte, domain.MaxTaskTitle+1))}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.service.Create(context.Background(), f.userID, tc.input); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCreateRecordsTheOriginatingRun(t *testing.T) {
	f := newFixture(t)
	runID := uuid.New()

	created, err := f.service.Create(context.Background(), f.userID, task.CreateInput{
		Title: "From an agent",
		RunID: &runID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.RunID == nil || *created.RunID != runID {
		t.Errorf("RunID = %v, want %s", created.RunID, runID)
	}
}

func TestUpdateAppliesOnlySuppliedFields(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, "Original")

	title := "  Renamed  "
	priority := domain.TaskPriorityHigh

	updated, err := f.service.Update(context.Background(), f.userID, original.ID, task.UpdateInput{
		Title:    &title,
		Priority: &priority,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.Title != "Renamed" {
		t.Errorf("Title = %q, want Renamed", updated.Title)
	}

	if updated.Priority != domain.TaskPriorityHigh {
		t.Errorf("Priority = %q, want high", updated.Priority)
	}

	if updated.Description != original.Description {
		t.Errorf("Description = %q, want it untouched", updated.Description)
	}

	if updated.Status != domain.TaskStatusPending {
		t.Errorf("Status = %q, want it untouched at pending", updated.Status)
	}
}

func TestUpdateRejectsInvalidValues(t *testing.T) {
	f := newFixture(t)
	original := f.create(t, "Original")

	blank := "   "
	priority := domain.TaskPriority("urgent")

	if _, err := f.service.Update(context.Background(), f.userID, original.ID, task.UpdateInput{Title: &blank}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("blank title error = %v, want ErrInvalid", err)
	}

	if _, err := f.service.Update(context.Background(), f.userID, original.ID, task.UpdateInput{Priority: &priority}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("bad priority error = %v, want ErrInvalid", err)
	}
}

func TestClearDueAtRemovesTheDate(t *testing.T) {
	f := newFixture(t)

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

	created, err := f.service.Create(context.Background(), f.userID, task.CreateInput{
		Title: "Has a due date",
		DueAt: &due,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.DueAt == nil {
		t.Fatal("Create must store the due date")
	}

	// An explicit clear is needed because a nil DueAt means "leave it alone".
	cleared, err := f.service.Update(context.Background(), f.userID, created.ID, task.UpdateInput{ClearDueAt: true})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if cleared.DueAt != nil {
		t.Errorf("DueAt = %v, want nil after clearing", cleared.DueAt)
	}
}

func TestCompleteStampsTheCompletionTime(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Completable")

	*f.clock = f.clock.Add(2 * time.Hour)

	completed, err := f.service.Complete(context.Background(), f.userID, created.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if completed.Status != domain.TaskStatusCompleted {
		t.Errorf("Status = %q, want completed", completed.Status)
	}

	if completed.CompletedAt == nil {
		t.Fatal("Complete must stamp CompletedAt")
	}

	if !completed.CompletedAt.Equal(*f.clock) {
		t.Errorf("CompletedAt = %s, want %s", completed.CompletedAt, *f.clock)
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Retryable")

	first, err := f.service.Complete(context.Background(), f.userID, created.ID)
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}

	*f.clock = f.clock.Add(time.Hour)

	second, err := f.service.Complete(context.Background(), f.userID, created.ID)
	if err != nil {
		t.Fatalf("an agent retrying Complete must not fail, got %v", err)
	}

	// The original completion time is the truth, so a retry must not move it.
	if !second.CompletedAt.Equal(*first.CompletedAt) {
		t.Errorf("CompletedAt moved from %s to %s on retry", *first.CompletedAt, *second.CompletedAt)
	}
}

func TestCancelledTaskCannotBeCompleted(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Doomed")

	cancelled, err := f.service.SetStatus(context.Background(), f.userID, created.ID, domain.TaskStatusCancelled)
	if err != nil {
		t.Fatalf("SetStatus(cancelled): %v", err)
	}

	if cancelled.Status != domain.TaskStatusCancelled {
		t.Fatalf("Status = %q, want cancelled", cancelled.Status)
	}

	_, err = f.service.Complete(context.Background(), f.userID, created.ID)
	if !errors.Is(err, domain.ErrStateTransition) {
		t.Errorf("completing a cancelled task error = %v, want ErrStateTransition", err)
	}
}

func TestCompletedTaskCannotBeCancelled(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Finished")

	if _, err := f.service.Complete(context.Background(), f.userID, created.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Cancelling would erase the fact that the work was done.
	_, err := f.service.SetStatus(context.Background(), f.userID, created.ID, domain.TaskStatusCancelled)
	if !errors.Is(err, domain.ErrStateTransition) {
		t.Errorf("cancelling a completed task error = %v, want ErrStateTransition", err)
	}
}

func TestTerminalTasksCannotReopen(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Finished")

	if _, err := f.service.Complete(context.Background(), f.userID, created.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for _, status := range []domain.TaskStatus{domain.TaskStatusPending, domain.TaskStatusInProgress} {
		_, err := f.service.SetStatus(context.Background(), f.userID, created.ID, status)
		if !errors.Is(err, domain.ErrStateTransition) {
			t.Errorf("moving a completed task to %q error = %v, want ErrStateTransition", status, err)
		}
	}
}

func TestPendingToInProgressIsAllowed(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Started")

	moved, err := f.service.SetStatus(context.Background(), f.userID, created.ID, domain.TaskStatusInProgress)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if moved.Status != domain.TaskStatusInProgress {
		t.Errorf("Status = %q, want in_progress", moved.Status)
	}

	if moved.CompletedAt != nil {
		t.Error("an in-progress task must not carry a completion time")
	}
}

func TestSetStatusRejectsUnknownStatus(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Confused")

	_, err := f.service.SetStatus(context.Background(), f.userID, created.ID, domain.TaskStatus("done-ish"))
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
}

func TestTaskFromAnotherUserIsInvisible(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "Private")

	other := uuid.New()

	if _, err := f.service.Get(context.Background(), other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Get error = %v, want ErrNotFound", err)
	}

	if _, err := f.service.Complete(context.Background(), other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Complete error = %v, want ErrNotFound", err)
	}

	if err := f.service.Delete(context.Background(), other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Delete error = %v, want ErrNotFound", err)
	}

	// The owner's task must be untouched.
	stored, err := f.service.Get(context.Background(), f.userID, created.ID)
	if err != nil {
		t.Fatalf("the owner's task must survive: %v", err)
	}

	if stored.Status != domain.TaskStatusPending {
		t.Errorf("Status = %q, want it still pending", stored.Status)
	}
}

func TestListIsScopedToTheCaller(t *testing.T) {
	f := newFixture(t)
	f.create(t, "Mine")

	// Another user's task, inserted directly so only the read path is tested.
	foreign := &domain.Task{ID: uuid.New(), UserID: uuid.New(), Title: "Theirs", Status: domain.TaskStatusPending}
	if err := f.repo.Create(context.Background(), foreign); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tasks, total, err := f.service.List(context.Background(), f.userID, domain.TaskFilter{}, domain.Page{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if total != 1 || len(tasks) != 1 || tasks[0].Title != "Mine" {
		t.Errorf("List returned %d tasks (total %d), want only the caller's", len(tasks), total)
	}
}
