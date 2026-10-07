package domain

import (
	"context"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/google/uuid"
)

// Page bounds a list query. Order is a column expression supplied by the
// caller; the implementations append it verbatim, so it must never be built
// from unvalidated request input.
type Page struct {
	Limit  int
	Offset int
	Order  string
}

// Normalise applies defaults and caps so a caller cannot request an unbounded
// page. It returns the effective values.
func (p Page) Normalise(maxLimit int) Page {
	if p.Limit <= 0 {
		p.Limit = 20
	}

	if p.Limit > maxLimit {
		p.Limit = maxLimit
	}

	if p.Offset < 0 {
		p.Offset = 0
	}

	return p
}

// The repositories below are the ports the application layer depends on. Every
// method that can touch user data takes a userID, and none of them offer a
// lookup by id alone. That is deliberate: making the tenant filter a required
// argument is what keeps a missing WHERE clause from becoming a data leak.
type UserRepository interface {
	Create(ctx context.Context, user *User) error

	// GetByID returns any user. Used by the auth service, which is not scoped
	// to a tenant because it is what establishes one.
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)

	// GetByEmail looks a user up for authentication.
	GetByEmail(ctx context.Context, email string) (*User, error)

	Update(ctx context.Context, user *User) error
}

type RefreshTokenRepository interface {
	Create(ctx context.Context, token *RefreshToken) error

	// GetByHash returns the token matching a plaintext token's hash.
	GetByHash(ctx context.Context, tokenHash string) (*RefreshToken, error)

	// RevokeFamily revokes every token in a family. Used when a spent token is
	// presented again.
	RevokeFamily(ctx context.Context, familyID uuid.UUID, now time.Time) error

	Update(ctx context.Context, token *RefreshToken) error
}

type OAuthTokenRepository interface {
	// Get returns the user's credential for one provider, or ErrNotFound.
	//
	// Tenant-scoped with no id-only lookup: a provider is not a tenant, and a
	// caller holding one must not be able to read another user's credential for it.
	Get(ctx context.Context, userID uuid.UUID, provider Provider) (*OAuthToken, error)

	// Save writes a credential, replacing any existing row for the same user and
	// provider so that a reconnect needs no separate upsert rule. The conflict is on
	// the user and provider pair, never on the id, so a reconnect cannot displace
	// another user's row.
	Save(ctx context.Context, token *OAuthToken) error
}

// NoteFilter narrows a note listing.
type NoteFilter struct {
	// Archived distinguishes archived notes from live ones. The zero value
	// means live only, which is what a default listing should return.
	Archived bool
	// IncludeArchived lists both live and archived notes.
	IncludeArchived bool
	Tags            []string
	Search          string
}

type NoteRepository interface {
	Create(ctx context.Context, note *Note) error

	// GetByID returns the note only if it belongs to userID.
	GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*Note, error)

	List(ctx context.Context, userID uuid.UUID, filter NoteFilter, page Page) ([]Note, error)

	Count(ctx context.Context, userID uuid.UUID, filter NoteFilter) (int64, error)

	// ReplaceTags swaps the note's tags atomically.
	ReplaceTags(ctx context.Context, userID uuid.UUID, noteID uuid.UUID, tags []string) error

	Update(ctx context.Context, note *Note) error

	// UpdateSearchState persists only a note's index bookkeeping.
	//
	// It is separate from Update because the two happen at different times and for
	// different reasons: Update carries an edit the user asked for, while this
	// records what the indexer then made of it. Writing the whole row here would
	// overwrite a concurrent edit with a stale copy read before the embedding calls
	// started.
	//
	// It returns ErrStale when the note changed since it was read, which is the
	// normal outcome of an edit that lands while an index attempt is in flight. The
	// stored row is the newer one and is already queued, so the caller discards its
	// write rather than retrying or reporting it.
	UpdateSearchState(ctx context.Context, note *Note) error

	// ClaimDueNotesForIndexing leases the notes that are due to be indexed and
	// returns them with the text to index.
	//
	// It marks each claim 'indexing' and pushes its next attempt out by the lease in
	// the same statement, so concurrent workers cannot pick the same note and a
	// worker that dies mid-index releases its note when the lease expires rather
	// than leaving it stuck. Returns at most claim.Limit notes, oldest first.
	ClaimDueNotesForIndexing(ctx context.Context, claim IndexClaim) ([]Note, error)

	Delete(ctx context.Context, userID uuid.UUID, id uuid.UUID) error
}

// IndexClaim describes one pass over the index queue.
type IndexClaim struct {
	// Now is the instant the queue is evaluated against, supplied by the caller so
	// tests and replays do not depend on the wall clock.
	Now time.Time

	// Limit bounds how many notes one pass claims, so a burst of edits cannot turn
	// into an unbounded run of embedding calls that outlives the process's patience.
	Limit int

	// Lease is how long a claimed note stays untouchable by another worker. It has
	// to exceed the slowest embedding call the runtime can make, or a healthy worker
	// would have its note stolen while it is still working on it.
	Lease time.Duration

	// MaxAttempts is where a note stops being retried. Without a cap, a note whose
	// embedding runtime is permanently broken is retried for the lifetime of the
	// deployment, and a fix never reaches the notes that needed it.
	MaxAttempts int
}

type NoteChunkRepository interface {
	// ReplaceChunks swaps all chunks for a note in one call, which keeps the
	// stored chunks consistent with the note text.
	//
	// Every chunk must belong to the same note and the same user, and the batch
	// must be non-empty. An empty slice cannot name a note, so it would leave the
	// old chunks in place while appearing to succeed; clearing is DeleteByNote's
	// job.
	ReplaceChunks(ctx context.Context, chunks []NoteChunk) error

	ListByNote(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) ([]NoteChunk, error)

	DeleteByNote(ctx context.Context, userID uuid.UUID, noteID uuid.UUID) error

	// Search returns the user's chunks closest to query, ordered nearest first,
	// excluding anything further than minDistance. The user filter is not optional:
	// this is the one query whose candidates come from a shared vector index.
	Search(
		ctx context.Context,
		userID uuid.UUID,
		query Vector,
		limit int,
		minDistance float64,
	) ([]NoteChunkMatch, error)
}

// NoteChunkMatch is a chunk that matched a search, with its distance.
//
// Distance is cosine distance, so lower is nearer, and is carried through instead
// of a precomputed similarity so a caller can see the number it filtered on.
type NoteChunkMatch struct {
	NoteChunk

	Distance float64 `json:"distance"`
}

// Similarity converts cosine distance to a 0..1 score.
func (m NoteChunkMatch) Similarity() float64 { return 1 - m.Distance }

// Vector is the embedding type used at the port boundary. It is an alias so
// callers can pass a pgvector.Vector without the domain package's contracts
// depending on a driver.
type Vector = pgvector.Vector

// TaskFilter narrows a task listing.
type TaskFilter struct {
	Status   TaskStatus
	Priority TaskPriority
	// DueBefore and DueAfter bound the due date, and are nil when unbounded.
	DueBefore *time.Time
	DueAfter  *time.Time
	// RunID lists only tasks a given agent run created.
	RunID *uuid.UUID
}

type TaskRepository interface {
	Create(ctx context.Context, task *Task) error

	// GetByID returns the task only if it belongs to userID.
	GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*Task, error)

	List(ctx context.Context, userID uuid.UUID, filter TaskFilter, page Page) ([]Task, error)

	Count(ctx context.Context, userID uuid.UUID, filter TaskFilter) (int64, error)

	Update(ctx context.Context, task *Task) error

	Delete(ctx context.Context, userID uuid.UUID, id uuid.UUID) error
}

// ConversationRepository stores chat threads.
//
// Reads are scoped to a user id in SQL rather than by checking a row in Go, so
// there is no path where a missing check becomes a leak of somebody else's
// conversation.
type ConversationRepository interface {
	Create(ctx context.Context, conversation *Conversation) error

	// GetByID returns the conversation only if it belongs to userID.
	GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*Conversation, error)

	// List returns the user's conversations, most recently touched first.
	List(ctx context.Context, userID uuid.UUID, limit int, offset int) ([]Conversation, error)

	Count(ctx context.Context, userID uuid.UUID) (int64, error)

	// Update persists the conversation's title and head pointer.
	Update(ctx context.Context, conversation *Conversation) error

	Delete(ctx context.Context, userID uuid.UUID, id uuid.UUID) error
}

// MessageRepository stores conversation turns.
type MessageRepository interface {
	Create(ctx context.Context, message *Message) error

	// GetByID returns the message only if it belongs to userID.
	GetByID(ctx context.Context, userID uuid.UUID, id uuid.UUID) (*Message, error)

	// Update persists the mutable fields of a message: its content, reasoning and
	// the run it produced.
	Update(ctx context.Context, message *Message) error

	// ListActivePath returns the messages on the conversation's current branch, in
	// order, excluding superseded ones.
	//
	// "Active path" rather than "every message", because an edited thread is a
	// tree: the messages after the edit still exist and are still readable, but
	// showing them above the correction would put the answer before the question
	// it answers.
	ListActivePath(ctx context.Context, userID uuid.UUID, conversationID uuid.UUID) ([]Message, error)

	// ListAll returns every message in the conversation, including superseded ones.
	ListAll(ctx context.Context, userID uuid.UUID, conversationID uuid.UUID) ([]Message, error)

	Delete(ctx context.Context, userID uuid.UUID, conversationID uuid.UUID) error
}
