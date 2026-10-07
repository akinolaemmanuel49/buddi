DROP INDEX IF EXISTS notes_pending_index_idx;

ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_grounding_state_check,
    DROP COLUMN IF EXISTS grounding_state;

ALTER TABLE notes
    DROP CONSTRAINT IF EXISTS notes_search_state_check,
    DROP COLUMN IF EXISTS indexed_at,
    DROP COLUMN IF EXISTS next_index_at,
    DROP COLUMN IF EXISTS last_index_error,
    DROP COLUMN IF EXISTS index_attempts,
    DROP COLUMN IF EXISTS search_state;