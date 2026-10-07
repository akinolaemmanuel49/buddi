DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS approvals;
DROP TABLE IF EXISTS agent_runs;
DROP TABLE IF EXISTS note_chunks;
DROP TABLE IF EXISTS notes;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;

-- The vector and citext extensions are left in place. Other databases in the
-- cluster may depend on them, and dropping them would fail in that case.