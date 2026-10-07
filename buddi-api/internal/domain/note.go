package domain

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// Note length limits. Content is unbounded on purpose since it is the primary
// input to RAG, but the title has to stay a title.
const (
	MaxNoteTitle     = 300
	MaxNoteTags      = 25
	MaxTagLength     = 50
	NoteSourceManual = "manual"
)

// Note is a piece of personal information the user has stored.
type Note struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	Title  string    `gorm:"type:text;not null" json:"title"`
	// Content holds the note body. JSON newlines are stored literally, which
	// keeps the text as written for both display and embedding.
	Content    string     `gorm:"type:text;not null" json:"content"`
	Tags       TagList    `gorm:"type:text[];not null" json:"tags"`
	Source     string     `gorm:"type:text;not null" json:"source"`
	ArchivedAt *time.Time `gorm:"type:timestamptz" json:"archived_at"`
	// SearchState and its neighbours record whether this note's chunks are present
	// and current. They are exposed because "why doesn't my note come up in
	// search" is otherwise unanswerable, not because a client should drive them.
	SearchState    SearchState `gorm:"type:text;not null;default:pending" json:"search_state"`
	IndexAttempts  int         `gorm:"not null;default:0" json:"-"`
	LastIndexError string      `gorm:"type:text" json:"-"`
	NextIndexAt    time.Time   `gorm:"type:timestamptz;not null" json:"-"`
	IndexedAt      *time.Time  `gorm:"type:timestamptz" json:"indexed_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

func (Note) TableName() string { return "notes" }

// SearchState tracks whether a note's embedded chunks are present and current.
//
// It is stored rather than derived because the state that matters most is the one
// with no chunks at all, which is indistinguishable from "never searched for" if you
// infer it from the absence of rows.
type SearchState string

const (
	// SearchStatePending means the note's text has changed, or has never been
	// indexed, and is waiting to be embedded.
	SearchStatePending SearchState = "pending"

	// SearchStateIndexing means a worker has claimed the note and is embedding it.
	// It exists so a second worker does not claim the same note, not to be shown to
	// users.
	SearchStateIndexing SearchState = "indexing"

	// SearchStateIndexed means the stored chunks match the current text.
	SearchStateIndexed SearchState = "indexed"

	// SearchStateFailed means indexing gave up after repeated attempts. The note is
	// still readable and still exportable; it is only not searchable, and the last
	// error is kept so the cause is not lost.
	SearchStateFailed SearchState = "failed"
)

// Searchable reports whether the note can currently be found by search.
//
// An archived note stays searchable on purpose: archiving hides it from the list,
// it does not retract what the user wrote, and a plan grounded in an old note is
// still grounded in something they actually said.
func (n *Note) Searchable() bool {
	return n != nil && n.SearchState == SearchStateIndexed
}

// MarkIndexing claims the note for a worker.
func (n *Note) MarkIndexing() {
	n.SearchState = SearchStateIndexing
}

// MarkIndexed records a successful index.
func (n *Note) MarkIndexed(now time.Time) {
	n.SearchState = SearchStateIndexed
	n.IndexedAt = &now
	n.LastIndexError = ""
}

// MarkIndexFailed records a failed attempt and the delay before the next one.
//
// The attempt count is kept so backoff can grow and so a note that keeps failing
// can be told apart from one that hit a single transient error. The error text is
// retained because "not searchable" with no reason is not actionable.
func (n *Note) MarkIndexFailed(err string, attempts int, retryAt time.Time) {
	n.SearchState = SearchStateFailed
	n.IndexAttempts = attempts
	n.LastIndexError = err
	n.NextIndexAt = retryAt
}

// MarkIndexPending queues the note for indexing.
//
// Every write of note text calls this, which is what makes "the note was saved" and
// "the note is queued to be searchable" the same commit: the flag travels on the
// note row, so there is no window where one happened without the other.
func (n *Note) MarkIndexPending(now time.Time) {
	n.SearchState = SearchStatePending
	n.NextIndexAt = now
	n.IndexedAt = nil
}

// Archived reports whether the note has been archived.
func (n *Note) Archived() bool { return n.ArchivedAt != nil }

// NewNote validates and normalises a note ready to be stored.
func NewNote(userID uuid.UUID, title string, content string, tags []string, source string, now time.Time) (*Note, error) {
	trimmedTitle := strings.TrimSpace(title)
	if trimmedTitle == "" {
		return nil, Invalid("title is required", map[string]string{"title": "must not be empty"})
	}

	if utf8.RuneCountInString(trimmedTitle) > MaxNoteTitle {
		return nil, Invalid("title is too long", map[string]string{
			"title": "must be at most " + strconv.Itoa(MaxNoteTitle) + " characters",
		})
	}

	if strings.TrimSpace(content) == "" {
		return nil, Invalid("content is required", map[string]string{"content": "must not be empty"})
	}

	normalisedTags, err := NormaliseTags(tags)
	if err != nil {
		return nil, err
	}

	if source == "" {
		source = NoteSourceManual
	}

	return &Note{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     trimmedTitle,
		Content:   content,
		Tags:      TagListOf(normalisedTags),
		Source:    source,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Archive marks the note archived without deleting it, so retrieval can exclude
// it while the content stays recoverable.
func (n *Note) Archive(now time.Time) {
	if n.ArchivedAt == nil {
		stamp := now
		n.ArchivedAt = &stamp
	}
}

// Restore reverses Archive.
func (n *Note) Restore() { n.ArchivedAt = nil }

// NormaliseTags lowercases, trims, de-duplicates and sorts tags so that
// ["Work", "work", " Work "] always collapses to ["work"].
func NormaliseTags(tags []string) ([]string, error) {
	if len(tags) > MaxNoteTags {
		return nil, Invalid("too many tags", map[string]string{
			"tags": "must contain at most " + strconv.Itoa(MaxNoteTags) + " values",
		})
	}

	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))

	for _, tag := range tags {
		trimmed := strings.ToLower(strings.TrimSpace(tag))
		if trimmed == "" {
			continue
		}

		if utf8.RuneCountInString(trimmed) > MaxTagLength {
			return nil, Invalid("a tag is too long", map[string]string{
				"tags": "each tag must be at most " + strconv.Itoa(MaxTagLength) + " characters",
			})
		}

		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}

		seen[trimmed] = struct{}{}

		out = append(out, trimmed)
	}

	// Sorting keeps tag filters predictable and makes equality checks in tests
	// straightforward.
	sort.Strings(out)

	return out, nil
}

// NoteChunk is a slice of a note that has been embedded for retrieval.
//
// UserID is denormalised from the parent note so a similarity search can filter
// by owner without joining, which keeps the HNSW scan on one table.
type NoteChunk struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	NoteID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:note_chunks_note_ordinal" json:"note_id"`
	UserID     uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	Ordinal    int       `gorm:"not null;uniqueIndex:note_chunks_note_ordinal" json:"ordinal"`
	Content    string    `gorm:"type:text;not null" json:"content"`
	TokenCount int       `gorm:"not null" json:"token_count"`
	// Embedding is nil until the chunk has been embedded. A nil vector is stored
	// as SQL NULL, which pgvector excludes from similarity search.
	Embedding pgvector.Vector `gorm:"type:vector(768)" json:"-"`
	CreatedAt time.Time       `json:"created_at"`
}

func (NoteChunk) TableName() string { return "note_chunks" }
