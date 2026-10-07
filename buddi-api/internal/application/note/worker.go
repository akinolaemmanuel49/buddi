package note

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// Worker turns the index queue on the notes table into searchable chunks, away from
// the request that wrote them.
//
// It exists because embedding is the one slow thing a note write does: a note of a
// few thousand words is several sequential HTTP calls to the embedding runtime, and
// doing that inside the request makes saving a note depend on a second service being
// up and fast. The note write already records that the note is waiting, so the work
// has somewhere to go other than the request.
//
// Safety comes from the repository's claim rather than from anything here. A claim
// leases a note to one worker, so several can drain the queue at once, and a worker
// that dies leaves a lease that expires instead of a note stuck mid-index.
type Worker struct {
	bookkeeping indexBookkeeping
	indexer     Indexer
	opts        WorkerOptions
	log         *slog.Logger
}

// WorkerOptions configures the indexing loop.
type WorkerOptions struct {
	// BatchSize is how many notes one pass claims. Small enough that a pass finishes
	// quickly, large enough that a backlog is not drained a note at a time.
	BatchSize int

	// Interval is how long the loop waits when a pass finds nothing to do. It is the
	// worst-case delay between a note being saved and being searchable, so it is the
	// knob that decides how responsive search feels.
	Interval time.Duration

	// Lease is how long a claimed note stays untouchable. It must exceed the slowest
	// embedding call this runtime can make, or a healthy worker loses its note to
	// another worker halfway through and both pay for it.
	Lease time.Duration

	// MaxAttempts is where a note stops being retried. Zero uses the same limit the
	// inline path applies.
	MaxAttempts int

	// Clock is injectable for deterministic tests. Nil uses time.Now.
	Clock func() time.Time
}

// Default worker settings, chosen for a single-process deployment on one machine.
const (
	defaultIndexBatchSize = 20
	defaultIndexInterval  = 5 * time.Second
	// Generous relative to a local embedding call and short relative to the backoff
	// ceiling, so a crashed worker's notes are retried while they still matter to
	// whoever wrote them.
	defaultIndexLease = 2 * time.Minute
)

func (o WorkerOptions) withDefaults() WorkerOptions {
	if o.BatchSize <= 0 {
		o.BatchSize = defaultIndexBatchSize
	}

	if o.Interval <= 0 {
		o.Interval = defaultIndexInterval
	}

	if o.Lease <= 0 {
		o.Lease = defaultIndexLease
	}

	if o.MaxAttempts <= 0 {
		o.MaxAttempts = maxIndexAttempts
	}

	if o.Clock == nil {
		o.Clock = time.Now
	}

	return o
}

// NewWorker builds the indexing worker.
func NewWorker(notes domain.NoteRepository, indexer Indexer, opts WorkerOptions) (*Worker, error) {
	if notes == nil {
		return nil, errors.New("note: worker requires a note repository")
	}

	if indexer == nil {
		return nil, errors.New("note: worker requires an indexer")
	}

	opts = opts.withDefaults()

	return &Worker{
		bookkeeping: indexBookkeeping{notes: notes, now: opts.Clock},
		indexer:     indexer,
		opts:        opts,
	}, nil
}

// WithSearchStateObserver attaches a reporter for failed search-state writes, so the
// worker reports through the same channel the inline path does.
func (w *Worker) WithSearchStateObserver(observer SearchStateObserver) *Worker {
	w.bookkeeping.observer = observer
	return w
}

// WithLogger attaches a logger for the worker's own failures: a pass that cannot even
// claim its batch, which no observer would otherwise see.
func (w *Worker) WithLogger(log *slog.Logger) *Worker {
	w.log = log
	return w
}

// RunOnce claims and indexes a single batch, returning how many notes it finished.
//
// Exposed separately from Run because a loop that cannot be stepped cannot be tested
// for the thing that matters: that the second pass finds nothing when the first pass
// already indexed everything it claimed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	claimed, err := w.bookkeeping.notes.ClaimDueNotesForIndexing(ctx, domain.IndexClaim{
		Now:         w.bookkeeping.now(),
		Limit:       w.opts.BatchSize,
		Lease:       w.opts.Lease,
		MaxAttempts: w.opts.MaxAttempts,
	})
	if err != nil {
		return 0, err
	}

	indexed := 0

	for i := range claimed {
		// Checked between notes rather than only at the top of the pass so a
		// shutdown does not wait out a batch of embedding calls it no longer needs.
		if err := ctx.Err(); err != nil {
			return indexed, err
		}

		if w.indexOne(ctx, &claimed[i]) {
			indexed++
		}
	}

	return indexed, nil
}

// indexOne indexes a claimed note and records the outcome, reporting success.
//
// The note was claimed before the embedding calls started, so an edit in that window
// leaves the chunks describing text that is no longer there. The repository refuses
// the state write for exactly that case, and the edit's own queueing stands, so the
// note is rebuilt on a later pass instead of being labelled searchable with the wrong
// content.
func (w *Worker) indexOne(ctx context.Context, claimed *domain.Note) bool {
	err := w.indexer.Index(ctx, claimed)

	// One note's failure must not end the batch. The backoff keeps the next pass
	// from retrying it immediately, and the remaining notes have nothing to do with
	// why this one failed.
	w.bookkeeping.finish(ctx, claimed, err, w.bookkeeping.now())

	return err == nil
}

// Run drains the queue until ctx is cancelled.
//
// It never returns an error: the loop is the durable half of a best-effort capability,
// so a failure to claim work is logged and retried at the next interval rather than
// taking the process down or exiting and leaving the queue for someone to restart.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	for {
		indexed, err := w.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Cancelling is how this loop is asked to stop.
			return
		case err != nil:
			w.logWarn("could not claim notes for indexing", "error", err)
		case indexed > 0:
			w.logInfo("indexed notes", "count", indexed)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) logWarn(message string, args ...any) {
	if w.log != nil {
		w.log.Warn(message, args...)
	}
}

func (w *Worker) logInfo(message string, args ...any) {
	if w.log != nil {
		w.log.Info(message, args...)
	}
}
