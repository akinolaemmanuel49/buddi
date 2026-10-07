-- Restore the narrower predicate from migration 000003: a note that is not
-- queued for indexing is not something the claim query looks for.
--
-- Safe in either direction. 'indexing' rows left behind by this rollback are
-- still due the moment their lease expires, because they are due by state and
-- time rather than by membership of an index.
DROP INDEX IF EXISTS notes_pending_index_idx;

CREATE INDEX notes_pending_index_idx ON notes (next_index_at)
    WHERE search_state IN ('pending', 'failed');