package gormdb

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// NoteChunkRepository stores embedded note slices.
//
// The tenant filter is applied in SQL for every method, including search. That is
// not defensive repetition: search is the one query whose result set comes from
// a vector index, and an index scan that forgot the owner filter would return
// another user's notes to whoever asked a similar question.
type NoteChunkRepository struct {
	db *gorm.DB
}

var _ domain.NoteChunkRepository = (*NoteChunkRepository)(nil)

func NewNoteChunkRepository(db *gorm.DB) *NoteChunkRepository {
	return &NoteChunkRepository{db: db}
}

func (r *NoteChunkRepository) conn(ctx context.Context) *gorm.DB {
	if r == nil || r.db == nil {
		return nil
	}

	if tx, ok := TxFromContext(ctx); ok {
		return tx.Conn().WithContext(ctx)
	}

	return r.db.WithContext(ctx)
}

// ReplaceChunks swaps every chunk for a note inside one transaction, so a reader
// never observes a note whose chunks are half from the old text and half from the
// new. Deleting and inserting separately without a transaction is the failure
// mode this exists to prevent.
func (r *NoteChunkRepository) ReplaceChunks(ctx context.Context, chunks []domain.NoteChunk) error {
	db := r.conn(ctx)
	if db == nil {
		return errNotConnected
	}

	if len(chunks) == 0 {
		// An empty batch names no note, so clearing is DeleteByNote's job. Silently
		// succeeding here would leave stale chunks in place while appearing to have
		// replaced them.
		return errors.New("gormdb: ReplaceChunks requires at least one chunk")
	}

	noteID, userID := chunks[0].NoteID, chunks[0].UserID

	for _, chunk := range chunks[1:] {
		// Refuse a mixed batch rather than deleting one note's chunks and
		// inserting another's. This is a programming error, not bad user input, so
		// it stays an opaque internal error rather than becoming a 4xx.
		if chunk.NoteID != noteID || chunk.UserID != userID {
			return errors.New("gormdb: ReplaceChunks called with chunks from more than one note")
		}
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).
			Where("note_id = ? AND user_id = ?", noteID, userID).
			Delete(&domain.NoteChunk{}).Error; err != nil {
			return translate(err)
		}

		if err := tx.WithContext(ctx).Create(&chunks).Error; err != nil {
			return translate(err)
		}

		return nil
	})
}

// ListByNote returns a note's chunks in reading order.
func (r *NoteChunkRepository) ListByNote(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) ([]domain.NoteChunk, error) {
	db := r.conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	chunks := make([]domain.NoteChunk, 0)

	if err := db.Where("user_id = ?", userID).
		Where("note_id = ?", noteID).
		Order("ordinal ASC").
		Find(&chunks).Error; err != nil {
		return nil, translate(err)
	}

	return chunks, nil
}

func (r *NoteChunkRepository) DeleteByNote(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) error {
	db := r.conn(ctx)
	if db == nil {
		return errNotConnected
	}

	if err := db.Where("user_id = ?", userID).
		Where("note_id = ?", noteID).
		Delete(&domain.NoteChunk{}).Error; err != nil {
		return translate(err)
	}

	return nil
}

// Search returns the chunks closest to query, restricted to userID.
//
// The tenant filter is part of the same statement as the ordering, so it is
// applied before the index returns candidates rather than after.
func (r *NoteChunkRepository) Search(
	ctx context.Context,
	userID uuid.UUID,
	query domain.Vector,
	limit int,
	minDistance float64,
) ([]domain.NoteChunkMatch, error) {
	db := r.conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	if limit <= 0 {
		return []domain.NoteChunkMatch{}, nil
	}

	results := make([]domain.NoteChunkMatch, 0)

	// Written as raw SQL rather than through the query builder because the
	// distance has to appear three times: once selected, once filtered, once
	// ordered. pgvector.Vector implements driver.Valuer, so it binds as a
	// parameter and the statement stays injection-safe.
	//
	// A nil embedding is stored as SQL NULL, which pgvector excludes from
	// similarity search anyway; the IS NOT NULL clause states the intent rather
	// than relying on that behaviour.
	const searchSQL = `
		SELECT note_chunks.*, (note_chunks.embedding <=> ?) AS distance
		FROM note_chunks
		WHERE note_chunks.user_id = ?
		  AND note_chunks.embedding IS NOT NULL
		  AND (note_chunks.embedding <=> ?) <= ?
		ORDER BY note_chunks.embedding <=> ?
		LIMIT ?`

	if err := db.Raw(searchSQL, query, userID, query, minDistance, query, limit).
		Scan(&results).Error; err != nil {
		return nil, translate(err)
	}

	return results, nil
}
