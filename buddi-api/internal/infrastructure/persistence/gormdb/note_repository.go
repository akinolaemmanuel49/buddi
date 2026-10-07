package gormdb

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/persistence"
)

// NoteRepository stores notes. Every read and write is scoped to a user id, and
// the scope is applied in SQL rather than by reading a row and checking it in Go,
// so there is no path where a missing check becomes a leak.
type NoteRepository struct {
	*BaseRepository[domain.Note]
}

var _ domain.NoteRepository = (*NoteRepository)(nil)

func NewNoteRepository(db *gorm.DB) *NoteRepository {
	return &NoteRepository{BaseRepository: NewRepository[domain.Note](db)}
}

// GetByID returns the note only when it belongs to userID. A note owned by
// someone else reports ErrNotFound, not a permission error: distinguishing the
// two would confirm that the id exists.
func (r *NoteRepository) GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*domain.Note, error) {
	return r.First(ctx,
		persistence.Where("user_id = ?", userID),
		persistence.Where("id = ?", id),
	)
}

// List returns the user's notes matching filter.
func (r *NoteRepository) List(ctx context.Context, userID uuid.UUID, filter domain.NoteFilter, page domain.Page) ([]domain.Note, error) {
	notes := make([]domain.Note, 0)

	db := applyNoteFilter(r.Conn(ctx), userID, filter).Limit(page.Limit).Offset(page.Offset)
	if db == nil {
		return nil, errNotConnected
	}

	if err := orderNotes(db, page.Order).Find(&notes).Error; err != nil {
		return nil, translate(err)
	}

	return notes, nil
}

// Count returns how many notes match the filter, for pagination totals.
func (r *NoteRepository) Count(ctx context.Context, userID uuid.UUID, filter domain.NoteFilter) (int64, error) {
	db := applyNoteFilter(r.Conn(ctx), userID, filter)
	if db == nil {
		return 0, errNotConnected
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return 0, translate(err)
	}

	return total, nil
}

// ReplaceTags swaps a note's tags.
func (r *NoteRepository) ReplaceTags(ctx context.Context, userID uuid.UUID, noteID uuid.UUID, tags []string) error {
	result := r.Conn(ctx).Model(&domain.Note{}).
		Where("user_id = ? AND id = ?", userID, noteID).
		Update("tags", domain.TagListOf(tags))

	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

// Update writes a note. The owner id is part of the WHERE clause even though the
// entity carries it, so a mismatched in-memory object cannot rewrite someone else's
// row.
//
// Spelled out as SQL rather than built from the struct because GORM manages
// updated_at itself and overwrites the value it is given. That matters twice here:
// the service's clock is what created_at and next_index_at were written with, so an
// ORM-generated timestamp makes one note's columns disagree about when they were
// written, and it is the value UpdateSearchState compares against to detect an edit
// that landed mid-attempt. A guard against a timestamp nothing else agrees on would
// reject every write, or none.
func (r *NoteRepository) Update(ctx context.Context, note *domain.Note) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	// The search columns are included because the service queues the note in the same
	// statement that stores the edit. Leaving them out would save the text without
	// queueing it, which is the exact window this design exists to close.
	const updateNote = `
		UPDATE notes
		SET title = $1,
			content = $2,
			tags = $3::text[],
			archived_at = $4,
			updated_at = $5,
			search_state = $6,
			index_attempts = $7,
			last_index_error = $8,
			next_index_at = $9,
			indexed_at = $10
		WHERE id = $11 AND user_id = $12`

	result := db.Exec(updateNote,
		note.Title,
		note.Content,
		note.Tags,
		note.ArchivedAt,
		note.UpdatedAt,
		note.SearchState,
		note.IndexAttempts,
		note.LastIndexError,
		note.NextIndexAt,
		note.IndexedAt,
		note.ID,
		note.UserID,
	)

	if err := translate(result.Error); err != nil {
		return err
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

// UpdateSearchState persists only the index bookkeeping columns.
//
// It deliberately does not touch title, content, tags or archived_at. The note this
// is called with was read before the embedding calls began, so writing the whole row
// would silently revert any edit made in the meantime.
//
// The updated_at guard closes the other half of that race. Without it, an index
// attempt that started before an edit can finish after it and mark the note indexed
// with chunks built from the text that has just been replaced: the note claims to be
// searchable and is not, and nothing re-queues it because the state says it is done.
// Refusing the write leaves the newer version's own queueing intact, so the next
// attempt rebuilds the chunks from the current text.
//
// The write spells out updated_at for the same reason Update does: left to itself GORM
// would restamp it with the current time, making the guard compare a value nothing
// else in the system agrees on.
func (r *NoteRepository) UpdateSearchState(ctx context.Context, note *domain.Note) error {
	db := r.Conn(ctx)
	if db == nil {
		return errNotConnected
	}

	result := db.Exec(`UPDATE notes
		SET search_state = $1,
			index_attempts = $2,
			last_index_error = $3,
			next_index_at = $4,
			indexed_at = $5,
			updated_at = $6
		WHERE id = $7 AND user_id = $8 AND updated_at = $9`,
		note.SearchState,
		note.IndexAttempts,
		note.LastIndexError,
		note.NextIndexAt,
		note.IndexedAt,
		note.UpdatedAt,
		note.ID,
		note.UserID,
		note.UpdatedAt,
	)

	if err := translate(result.Error); err != nil {
		return err
	}

	if result.RowsAffected > 0 {
		return nil
	}

	// Distinguishing "gone" from "moved on" costs one extra query on a path that is
	// only reached when something changed underneath us. Collapsing them into
	// not-found would either delete notes that still exist or report a routine
	// concurrent edit as a missing row.
	var exists bool

	err := db.Raw(`SELECT EXISTS (SELECT 1 FROM notes WHERE id = $1 AND user_id = $2)`, note.ID, note.UserID).
		Scan(&exists).Error
	if err := translate(err); err != nil {
		return err
	}

	if exists {
		return persistence.ErrStale
	}

	return persistence.ErrNotFound
}

// ClaimDueNotesForIndexing leases a batch of notes that are due to be indexed.
//
// One statement, because the alternative is a select followed by an update and the
// gap between them is where two workers end up embedding the same note. The CTE
// locks the rows it picks and SKIP LOCKED leaves locked rows to whoever holds them,
// so the queue drains in parallel rather than queueing behind the first claim.
func (r *NoteRepository) ClaimDueNotesForIndexing(
	ctx context.Context,
	claim domain.IndexClaim,
) ([]domain.Note, error) {
	db := r.Conn(ctx)
	if db == nil {
		return nil, errNotConnected
	}

	// A zero limit would mean every note, which is the opposite of what a caller
	// asking for nothing intends.
	if claim.Limit <= 0 {
		return []domain.Note{}, nil
	}

	// 'indexing' is claimed alongside the other two states so a note whose worker
	// died is reclaimed by this same query once its lease expires, rather than by a
	// separate sweeper that could disagree about what is stuck.
	const claimSQL = `
		WITH claimable AS (
			SELECT notes.id
			FROM notes
			WHERE notes.search_state IN ('pending', 'failed', 'indexing')
			  AND notes.next_index_at <= $1
			  AND notes.index_attempts < $2
			ORDER BY notes.next_index_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notes n
		SET search_state = 'indexing',
			next_index_at = $4
		FROM claimable
		WHERE n.id = claimable.id
		RETURNING n.id, n.user_id, n.title, n.content, n.tags, n.source, n.archived_at,
			n.search_state, n.index_attempts, n.last_index_error, n.next_index_at, n.indexed_at,
			n.created_at, n.updated_at`

	// The lease is computed here rather than as an interval in SQL so the timestamp
	// the note is stamped with is the same value the worker reasoned about.
	leaseUntil := claim.Now.Add(claim.Lease)

	notes := make([]domain.Note, 0, claim.Limit)

	if err := db.WithContext(ctx).
		Raw(claimSQL, claim.Now, claim.MaxAttempts, claim.Limit, leaseUntil).
		Scan(&notes).Error; err != nil {
		return nil, translate(err)
	}

	return notes, nil
}

// Delete removes the note only if it belongs to userID. Chunks are removed by the
// foreign key cascade.
func (r *NoteRepository) Delete(ctx context.Context, userID uuid.UUID, id uuid.UUID) error {
	result := r.Conn(ctx).Where("user_id = ? AND id = ?", userID, id).Delete(&domain.Note{})
	if result.Error != nil {
		return translate(result.Error)
	}

	if result.RowsAffected == 0 {
		return persistence.ErrNotFound
	}

	return nil
}

func applyNoteFilter(db *gorm.DB, userID uuid.UUID, filter domain.NoteFilter) *gorm.DB {
	if db == nil {
		return nil
	}

	db = db.Model(&domain.Note{}).Where("user_id = ?", userID)

	switch {
	case filter.IncludeArchived:
	case filter.Archived:
		db = db.Where("archived_at IS NOT NULL")
	default:
		db = db.Where("archived_at IS NULL")
	}

	if len(filter.Tags) > 0 {
		// Every requested tag must be present, which is overlap semantics rather
		// than the any-match default of &&. The value is a TagList, not a
		// []string, so it binds as a text[] parameter instead of a SQL tuple.
		db = db.Where("tags @> ?", domain.TagListOf(filter.Tags))
	}

	if search := strings.TrimSpace(filter.Search); search != "" {
		pattern := "%" + escapeLike(search) + "%"
		db = db.Where("title ILIKE ? OR content ILIKE ?", pattern, pattern)
	}

	return db
}

// escapeLike neutralises the LIKE metacharacters in a user supplied search term.
// Without this a search for "%" matches every note.
func escapeLike(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

	return replacer.Replace(term)
}

// noteOrderings maps the order values a client may send onto fixed SQL. The
// order expression is never interpolated from request input.
var noteOrderings = map[string]string{
	"":         "created_at DESC",
	"newest":   "created_at DESC",
	"oldest":   "created_at ASC",
	"updated":  "updated_at DESC",
	"title":    "title ASC",
	"titlerev": "title DESC",
}

func orderNotes(db *gorm.DB, order string) *gorm.DB {
	clause, ok := noteOrderings[strings.ToLower(strings.TrimSpace(order))]
	if !ok {
		clause = noteOrderings[""]
	}

	return db.Order(clause)
}
