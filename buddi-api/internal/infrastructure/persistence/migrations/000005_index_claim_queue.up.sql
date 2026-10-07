-- Widen the index queue to cover claimed notes.
--
-- Migration 000003 indexed only the notes nobody was working on: 'pending' and
-- 'failed'. A worker that claims a note moves it to 'indexing' and pushes its
-- next attempt out by the length of the lease, so those rows fall out of the
-- partial index for as long as the lease lasts.
--
-- That leaves two problems. A note whose worker died stays 'indexing' with no
-- index entry, so the query that would reclaim it has to scan the whole table to
-- find work it already knows is due. And the index no longer describes the queue:
-- it misses exactly the rows that are mid-flight.
--
-- Recreating the index is cheaper than reasoning about two indexes that between
-- them hold the queue. CREATE INDEX CONCURRENTLY would avoid taking a write lock,
-- but it cannot run inside a migration transaction, and on a table of notes there
-- is nothing to gain from being clever about it.
DROP INDEX IF EXISTS notes_pending_index_idx;

-- Ordered by next_index_at because that is what the claim query filters on, and
-- partial because the states left out are the ones nothing is ever looking for:
-- 'indexed' notes are done, and including them would make the index cover every
-- note in the table and defeat the point of tracking state at all.
CREATE INDEX notes_pending_index_idx ON notes (next_index_at)
    WHERE search_state IN ('pending', 'failed', 'indexing');