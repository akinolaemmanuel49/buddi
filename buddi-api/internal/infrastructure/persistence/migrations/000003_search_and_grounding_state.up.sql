-- Search index state on notes, and grounding state on agent runs.
--
-- Both exist for the same reason: an optional capability degraded silently is
-- indistinguishable from the capability working. A note whose embedding failed
-- looks exactly like a note nobody has searched for, and an ungrounded plan looks
-- exactly like one the model reasoned about from the user's notes. Each flag makes
-- one of those states visible instead of inferred.

-- Notes ---------------------------------------------------------------------

-- SearchState tracks whether a note's chunks are present and current.
--
-- It lives on notes rather than note_chunks because the state that matters most is
-- the one where no chunks exist at all. Deriving "needs indexing" from
-- NOT EXISTS (SELECT 1 FROM note_chunks ...) cannot tell a note that was never
-- indexed from one being indexed right now, so a sweeper would have to guess or
-- lock. A column removes the ambiguity.
--
-- The default is 'pending' so a row inserted by any path that has not thought
-- about indexing is queued rather than silently unsearchable.
ALTER TABLE notes
    ADD COLUMN search_state text NOT NULL DEFAULT 'pending',
    ADD COLUMN index_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN last_index_error text,
    ADD COLUMN next_index_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN indexed_at timestamptz;

-- next_index_at carries the retry backoff, so a runtime that is down does not get
-- hammered once per request. It is part of the claim query's predicate rather than
-- something the worker filters in memory, which keeps several workers from
-- selecting the same row.
--
-- The partial index serves that claim directly: only unindexed notes are ever
-- looked up this way, and indexing every note in the table to find work defeats
-- the point of tracking the state at all.
CREATE INDEX notes_pending_index_idx ON notes (next_index_at)
    WHERE search_state IN ('pending', 'failed');

-- The state set is closed in the database, not only in Go. A typo in a status
-- update would otherwise leave a note that no claim query can ever match.
ALTER TABLE notes
    ADD CONSTRAINT notes_search_state_check
    CHECK (search_state IN ('pending', 'indexing', 'indexed', 'failed'));

-- Agent runs ---------------------------------------------------------------

-- GroundingState records how much of the user's own notes reached the planner.
--
-- Three states rather than a boolean, because "searched and found nothing
-- relevant" and "could not search" are different facts with different causes: the
-- first means the user's notes do not cover the request, the second means search
-- is broken. Collapsing them into one flag makes a working system look broken and
-- a broken one look empty-handed, which is how a retrieval feature loses trust.
--
-- 'no_context' is the default because it is the honest starting point: nothing has
-- been retrieved yet.
ALTER TABLE agent_runs
    ADD COLUMN grounding_state text NOT NULL DEFAULT 'no_context',
    ADD CONSTRAINT agent_runs_grounding_state_check
    CHECK (grounding_state IN ('grounded', 'no_context', 'ungrounded'));