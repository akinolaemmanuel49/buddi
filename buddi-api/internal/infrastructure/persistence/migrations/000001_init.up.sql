-- Initial Buddi schema: identity, notes with vector search, tasks, and the
-- agent run ledger with its approval queue.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS citext;

-- Identity ------------------------------------------------------------------

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext NOT NULL UNIQUE,
    password_hash text NOT NULL,
    display_name  text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the token. The plaintext is only ever handed to the client.
    token_hash   text NOT NULL UNIQUE,
    -- Tokens issued from one login share a family so a replayed token can be
    -- traced back to every sibling it spawned.
    family_id    uuid NOT NULL,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    replaced_by  uuid REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    user_agent   text,
    ip_address   inet,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_id_idx ON refresh_tokens (family_id);

-- Notes and retrieval -------------------------------------------------------

CREATE TABLE notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title       text NOT NULL,
    content     text NOT NULL,
    tags        text[] NOT NULL DEFAULT '{}',
    source      text NOT NULL DEFAULT 'manual',
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notes_user_id_created_at_idx ON notes (user_id, created_at DESC);
CREATE INDEX notes_tags_idx ON notes USING gin (tags);
CREATE INDEX notes_user_id_archived_at_idx ON notes (user_id, archived_at)
    WHERE archived_at IS NULL;

CREATE TABLE note_chunks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    note_id     uuid NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    -- Denormalised from the parent note so retrieval can filter by owner
    -- without joining, which keeps the HNSW scan on a single table.
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ordinal     integer NOT NULL,
    content     text NOT NULL,
    token_count integer NOT NULL,
    -- 768 dimensions matches the nomic-embed-text v1.5 output. The check keeps
    -- a misconfigured embedder from writing vectors the index cannot serve.
    embedding   vector (768),
    created_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (note_id, ordinal),
    CHECK (vector_dims(embedding) = 768)
);

CREATE INDEX note_chunks_note_id_idx ON note_chunks (note_id);
CREATE INDEX note_chunks_user_id_idx ON note_chunks (user_id);

CREATE INDEX note_chunks_embedding_idx ON note_chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Agent ledger --------------------------------------------------------------

CREATE TABLE agent_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    goal        text NOT NULL,
    status      text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'planning', 'awaiting_approval', 'executing',
                          'completed', 'failed', 'cancelled')),
    plan        jsonb,
    result      jsonb,
    error       text,
    -- OpenTelemetry trace id, so a run can be correlated with its spans.
    trace_id    text,
    model       text,
    started_at  timestamptz,
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_runs_user_id_created_at_idx ON agent_runs (user_id, created_at DESC);
CREATE INDEX agent_runs_user_id_status_idx ON agent_runs (user_id, status);

CREATE TABLE approvals (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      uuid NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    step_index  integer NOT NULL,
    tool_name   text NOT NULL,
    arguments   jsonb NOT NULL,
    rationale   text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    resolved_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX approvals_run_id_idx ON approvals (run_id);
-- Partial index keeps the pending queue lookup cheap as completed rows pile up.
CREATE INDEX approvals_pending_idx ON approvals (user_id, created_at)
    WHERE status = 'pending';

-- Tasks ---------------------------------------------------------------------

CREATE TABLE tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title       text NOT NULL,
    description text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'in_progress', 'completed', 'cancelled')),
    priority    text NOT NULL DEFAULT 'normal'
        CHECK (priority IN ('low', 'normal', 'high')),
    due_at      timestamptz,
    completed_at timestamptz,
    source      text NOT NULL DEFAULT 'manual',
    run_id      uuid REFERENCES agent_runs (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tasks_user_id_status_idx ON tasks (user_id, status);
CREATE INDEX tasks_user_id_due_at_idx ON tasks (user_id, due_at)
    WHERE due_at IS NOT NULL;
CREATE INDEX tasks_run_id_idx ON tasks (run_id);