package gormdb

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// ConversationRepository stores chat threads in Postgres.
type ConversationRepository struct {
	*BaseRepository[domain.Conversation]
}

var _ domain.ConversationRepository = (*ConversationRepository)(nil)

func NewConversationRepository(db *gorm.DB) *ConversationRepository {
	return &ConversationRepository{BaseRepository: NewRepository[domain.Conversation](db)}
}

// GetByID returns the conversation only when it belongs to userID. A conversation
// owned by someone else reports ErrNotFound rather than a permission error:
// distinguishing the two would confirm that the id exists.
func (r *ConversationRepository) GetByID(
	ctx context.Context,
	userID uuid.UUID,
	id uuid.UUID,
) (*domain.Conversation, error) {
	return r.First(ctx,
		persistence.Where("user_id = ?", userID),
		persistence.Where("id = ?", id),
	)
}

func (r *ConversationRepository) List(
	ctx context.Context,
	userID uuid.UUID,
	limit int,
	offset int,
) ([]domain.Conversation, error) {
	conversations := make([]domain.Conversation, 0)

	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	// Most recently touched first, so the thread a user was last working in is the
	// one at the top of the list rather than wherever its creation date puts it.
	err := db.Where("user_id = ?", userID).
		Order("updated_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&conversations).Error
	if err != nil {
		return nil, translate(err)
	}

	return conversations, nil
}

func (r *ConversationRepository) Count(ctx context.Context, userID uuid.UUID) (int64, error) {
	db := r.Conn(ctx)
	if db == nil {
		return 0, errNotConnected
	}

	var total int64
	if err := db.Model(&domain.Conversation{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return 0, translate(err)
	}

	return total, nil
}

// Update persists title and head pointer.
//
// Both are written together because they are the conversation's whole mutable
// state: a title landing without its head would label one branch while the thread
// continued down another.
func (r *ConversationRepository) Update(ctx context.Context, conversation *domain.Conversation) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.Conversation{}).
		Where("user_id = ? AND id = ?", conversation.UserID, conversation.ID).
		Updates(map[string]any{
			"title":           conversation.Title,
			"head_message_id": conversation.HeadMessageID,
			"updated_at":      conversation.UpdatedAt,
		})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}

	return nil
}

func (r *ConversationRepository) Delete(
	ctx context.Context,
	userID uuid.UUID,
	id uuid.UUID,
) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	// Messages cascade from the conversation in the schema, so this is the whole
	// delete.
	result := db.Where("user_id = ? AND id = ?", userID, id).Delete(&domain.Conversation{})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// MessageRepository stores conversation turns in Postgres.
type MessageRepository struct {
	*BaseRepository[domain.Message]
}

var _ domain.MessageRepository = (*MessageRepository)(nil)

func NewMessageRepository(db *gorm.DB) *MessageRepository {
	return &MessageRepository{BaseRepository: NewRepository[domain.Message](db)}
}

func (r *MessageRepository) GetByID(
	ctx context.Context,
	userID uuid.UUID,
	id uuid.UUID,
) (*domain.Message, error) {
	return r.First(ctx,
		persistence.Where("user_id = ?", userID),
		persistence.Where("id = ?", id),
	)
}

func (r *MessageRepository) ListAll(
	ctx context.Context,
	userID uuid.UUID,
	conversationID uuid.UUID,
) ([]domain.Message, error) {
	return r.list(ctx, userID, conversationID, false)
}

// Update persists content, reasoning, the linked run and the pending question.
//
// The thread shape is not writable: parent_id and superseded_message_id define the
// branch, and changing them underneath a transcript would reorder history that has
// already been shown. Editing a message is a new row, not an update of these.
func (r *MessageRepository) Update(ctx context.Context, message *domain.Message) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Model(&domain.Message{}).
		Where("user_id = ? AND id = ?", message.UserID, message.ID).
		Updates(map[string]any{
			"content":               message.Content,
			"reasoning":             message.Reasoning,
			"run_id":                message.RunID,
			"clarification_request": message.ClarificationRequest,
		})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// ListActivePath returns the conversation's current branch.
//
// The walk is done in SQL rather than in Go because a thread is a tree of unknown
// depth: doing it in Go means one query per level, and a chain long enough to be
// interesting is a chain long enough to be slow.
func (r *MessageRepository) ListActivePath(
	ctx context.Context,
	userID uuid.UUID,
	conversationID uuid.UUID,
) ([]domain.Message, error) {
	return r.list(ctx, userID, conversationID, true)
}

func (r *MessageRepository) list(
	ctx context.Context,
	userID uuid.UUID,
	conversationID uuid.UUID,
	activeOnly bool,
) ([]domain.Message, error) {
	messages := make([]domain.Message, 0)

	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	query := db.Where("user_id = ? AND conversation_id = ?", userID, conversationID)

	if activeOnly {
		query = query.Where("superseded_message_id IS NULL")
	}

	// Follow parent_id from the conversation's head. superseded_message_id being
	// null is what keeps an edit's original from being walked into: it is still a
	// child of the same parent as the edit, so without that filter both branches
	// would be returned and the original would reappear beside its replacement.
	err := query.
		Order("created_at ASC, id ASC").
		Find(&messages).Error
	if err != nil {
		return nil, translate(err)
	}

	if !activeOnly {
		return messages, nil
	}

	// A superseded original shares a created_at with the edit that replaced it, so
	// created_at alone cannot separate the two branches. Order by ancestry instead:
	// keep only the messages reachable from the head.
	return activeBranch(messages), nil
}

// activeBranch keeps only the chain of ancestors of the newest message, which is
// the head of the conversation's current branch.
//
// A single pass over an id lookup rather than a query per level: a thread is a
// tree of unknown depth, so walking it one round trip at a time makes a long
// conversation a long conversation to render.
func activeBranch(messages []domain.Message) []domain.Message {
	if len(messages) == 0 {
		return messages
	}

	byID := make(map[uuid.UUID]domain.Message, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
	}

	keep := make(map[uuid.UUID]struct{}, len(messages))

	for id := messages[len(messages)-1].ID; ; {
		if _, seen := keep[id]; seen {
			// Defensive: the schema forbids a direct cycle and the service cannot
			// build one, so this is unreachable. Breaking beats recursing forever.
			break
		}

		keep[id] = struct{}{}

		parent, ok := byID[id]

		if !ok || parent.ParentID == nil {
			break
		}

		id = *parent.ParentID
	}

	ordered := make([]domain.Message, 0, len(keep))

	for _, message := range messages {
		if _, ok := keep[message.ID]; ok {
			ordered = append(ordered, message)
		}
	}

	return ordered
}

func (r *MessageRepository) Delete(
	ctx context.Context,
	userID uuid.UUID,
	conversationID uuid.UUID,
) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	if err := db.Where("user_id = ? AND conversation_id = ?", userID, conversationID).
		Delete(&domain.Message{}).Error; err != nil {
		return translate(err)
	}

	return nil
}
