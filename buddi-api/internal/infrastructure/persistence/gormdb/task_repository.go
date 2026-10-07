package gormdb

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// TaskRepository stores tasks, always scoped to a user.
type TaskRepository struct {
	*BaseRepository[domain.Task]
}

var _ domain.TaskRepository = (*TaskRepository)(nil)

func NewTaskRepository(db *gorm.DB) *TaskRepository {
	return &TaskRepository{BaseRepository: NewRepository[domain.Task](db)}
}

// GetByID returns the task only when it belongs to userID.
func (r *TaskRepository) GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*domain.Task, error) {
	return r.First(ctx,
		persistence.Where("user_id = ?", userID),
		persistence.Where("id = ?", id),
	)
}

func (r *TaskRepository) List(ctx context.Context, userID uuid.UUID, filter domain.TaskFilter, page domain.Page) ([]domain.Task, error) {
	tasks := make([]domain.Task, 0)

	db := applyTaskFilter(r.Conn(ctx), userID, filter).Limit(page.Limit).Offset(page.Offset)
	if db == nil {
		return nil, errNotConnected
	}

	if err := orderTasks(db, page.Order).Find(&tasks).Error; err != nil {
		return nil, translate(err)
	}

	return tasks, nil
}

func (r *TaskRepository) Count(ctx context.Context, userID uuid.UUID, filter domain.TaskFilter) (int64, error) {
	db := applyTaskFilter(r.Conn(ctx), userID, filter)
	if db == nil {
		return 0, errNotConnected
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return 0, translate(err)
	}

	return total, nil
}

// Update writes a task with the owner in the WHERE clause.
func (r *TaskRepository) Update(ctx context.Context, task *domain.Task) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.Task{}).
		Where("id = ? AND user_id = ?", task.ID, task.UserID).
		Select("title", "description", "status", "priority", "due_at", "completed_at", "run_id", "updated_at").
		Updates(task)

	if err := translate(result.Error); err != nil {
		return err
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

// Delete removes the task only if it belongs to userID.
func (r *TaskRepository) Delete(ctx context.Context, userID uuid.UUID, id uuid.UUID) error {
	result := r.Conn(ctx).Where("user_id = ? AND id = ?", userID, id).Delete(&domain.Task{})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

func applyTaskFilter(db *gorm.DB, userID uuid.UUID, filter domain.TaskFilter) *gorm.DB {
	if db == nil {
		return nil
	}

	db = db.Model(&domain.Task{}).Where("user_id = ?", userID)

	if filter.Status != "" {
		db = db.Where("status = ?", string(filter.Status))
	}

	if filter.Priority != "" {
		db = db.Where("priority = ?", string(filter.Priority))
	}

	if filter.DueAfter != nil {
		db = db.Where("due_at IS NOT NULL AND due_at >= ?", *filter.DueAfter)
	}

	if filter.DueBefore != nil {
		db = db.Where("due_at IS NOT NULL AND due_at <= ?", *filter.DueBefore)
	}

	if filter.RunID != nil {
		db = db.Where("run_id = ?", *filter.RunID)
	}

	return db
}

// taskOrderings maps client supplied order values onto fixed SQL.
var taskOrderings = map[string]string{
	"":         "status ASC, due_at ASC NULLS LAST, created_at DESC",
	"due":      "due_at ASC NULLS LAST, created_at DESC",
	"duedesc":  "due_at DESC NULLS LAST, created_at DESC",
	"newest":   "created_at DESC",
	"oldest":   "created_at ASC",
	"priority": "CASE priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END, created_at DESC",
}

func orderTasks(db *gorm.DB, order string) *gorm.DB {
	clause, ok := taskOrderings[strings.ToLower(strings.TrimSpace(order))]
	if !ok {
		clause = taskOrderings[""]
	}

	return db.Order(clause)
}
