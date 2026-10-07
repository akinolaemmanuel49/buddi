package gormdb

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// AgentRepository stores runs and their approvals, always scoped to a user.
//
// A run is written before it is planned and updated as it advances, so the
// repository has to support partial progress: there is no create-and-finish
// shape to lean on.
type AgentRepository struct {
	*BaseRepository[domain.AgentRun]
}

func NewAgentRepository(db *gorm.DB) *AgentRepository {
	return &AgentRepository{BaseRepository: NewRepository[domain.AgentRun](db)}
}

// CreateRun inserts a new run.
func (r *AgentRepository) CreateRun(ctx context.Context, run *domain.AgentRun) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	if err := db.Create(run).Error; err != nil {
		return translate(err)
	}

	return nil
}

// UpdateRun persists a run's lifecycle change.
//
// The update is scoped to the owner so a bug elsewhere cannot move another
// user's run, and it reports a missing row rather than silently succeeding.
func (r *AgentRepository) UpdateRun(ctx context.Context, run *domain.AgentRun) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.AgentRun{}).
		Where("id = ? AND user_id = ?", run.ID, run.UserID).
		Updates(runToUpdates(run))
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

// GetRun returns the run only when it belongs to userID.
func (r *AgentRepository) GetRun(ctx context.Context, userID uuid.UUID, runID uuid.UUID) (*domain.AgentRun, error) {
	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	var run domain.AgentRun

	err := db.
		Where("user_id = ? AND id = ?", userID, runID).
		First(&run).Error
	if err != nil {
		return nil, translate(err)
	}

	return &run, nil
}

// ListRuns returns the caller's runs, newest first.
func (r *AgentRepository) ListRuns(ctx context.Context, userID uuid.UUID, filter domain.RunFilter, page domain.Page) ([]domain.AgentRun, int64, error) {
	db := applyRunFilter(r.Conn(ctx), userID, filter)
	if db == nil {
		return nil, 0, errNotConnected
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, translate(err)
	}

	runs := make([]domain.AgentRun, 0)

	if err := db.Scopes(orderRuns(page.Order)).Limit(page.Limit).Offset(page.Offset).Find(&runs).Error; err != nil {
		return nil, 0, translate(err)
	}

	return runs, total, nil
}

// CreateApproval inserts a proposal.
func (r *AgentRepository) CreateApproval(ctx context.Context, approval *domain.Approval) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	if err := db.Create(approval).Error; err != nil {
		return translate(err)
	}

	return nil
}

// UpdateApproval persists a decision.
//
// The pending status is part of the WHERE clause, which is what makes a double
// click safe: the second update matches no rows and reports a conflict instead
// of re-running the mutation.
func (r *AgentRepository) UpdateApproval(ctx context.Context, approval *domain.Approval) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.Approval{}).
		Where("id = ? AND user_id = ? AND status = ?", approval.ID, approval.UserID, domain.ApprovalStatusPending).
		Updates(map[string]any{
			"status":      approval.Status,
			"resolved_at": approval.ResolvedAt,
		})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return persistence.ErrConflict
	}

	return nil
}

// GetApproval returns the approval only when it belongs to userID.
func (r *AgentRepository) GetApproval(ctx context.Context, userID uuid.UUID, approvalID uuid.UUID) (*domain.Approval, error) {
	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	var approval domain.Approval

	err := db.
		Where("user_id = ? AND id = ?", userID, approvalID).
		First(&approval).Error
	if err != nil {
		return nil, translate(err)
	}

	return &approval, nil
}

// ListApprovals returns a run's proposals in step order.
func (r *AgentRepository) ListApprovals(ctx context.Context, userID uuid.UUID, runID uuid.UUID) ([]domain.Approval, error) {
	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	approvals := make([]domain.Approval, 0)

	err := db.
		Where("user_id = ? AND run_id = ?", userID, runID).
		Order("step_index ASC, created_at ASC").
		Find(&approvals).Error
	if err != nil {
		return nil, translate(err)
	}

	return approvals, nil
}

// runToUpdates selects the lifecycle columns explicitly.
//
// A struct update would skip zero values, so a transition back to an empty error
// string or a nil result would silently not be written.
// runToUpdates lists every column the agent service mutates after the run row is
// created.
//
// The list is spelled out rather than derived from the struct on purpose. A run is
// inserted before it is planned, so any column left out of this map keeps the
// default it was inserted with: omitting grounding_state or plan_fallback did
// exactly that, and every run then reported itself as ungrounded regardless of
// what the planner actually retrieved. GORM's struct-based Save would write them,
// but it also writes columns a partial update has no business touching, and the
// WHERE clause here is what keeps an update inside its owner's run.
func runToUpdates(run *domain.AgentRun) map[string]any {
	return map[string]any{
		"status":          run.Status,
		"plan":            run.Plan,
		"result":          run.Result,
		"error":           run.Error,
		"model":           run.Model,
		"trace_id":        run.TraceID,
		"plan_fallback":   run.PlanFallback,
		"grounding_state": run.GroundingState,
		"started_at":      run.StartedAt,
		"finished_at":     run.FinishedAt,
		"updated_at":      run.UpdatedAt,
	}
}

func applyRunFilter(db *gorm.DB, userID uuid.UUID, filter domain.RunFilter) *gorm.DB {
	if db == nil {
		return nil
	}

	db = db.Model(&domain.AgentRun{}).Where("user_id = ?", userID)

	if filter.Status != "" {
		db = db.Where("status = ?", filter.Status)
	}

	return db
}

func orderRuns(order string) func(*gorm.DB) *gorm.DB {
	switch order {
	case "created_at":
		return func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") }
	case "oldest":
		return func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") }
	default:
		return func(db *gorm.DB) *gorm.DB { return db.Order("created_at DESC") }
	}
}
