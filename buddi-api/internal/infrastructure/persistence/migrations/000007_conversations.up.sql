-- Adds the conversation transcript behind the chat interface.
--
-- Previously the only unit of work was an AgentRun: one goal, one plan, one
-- approval. That shape cannot support revising a request, because there is
-- nowhere to put the revised wording or the turns around it. These two tables
-- supply that missing history, and the runs table now points at it.

-- Conversations -----------------------------------------------------------

-- A conversation is a thread of messages. It is a separate row rather than a
-- derived grouping so that a title, an ordering and a deletion boundary have
-- somewhere to live; deriving all three from the messages themselves would mean
-- recomputing them on every read.
CREATE TABLE conversations (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Nullable because a conversation starts with no messages and therefore has
    -- nothing to be called. It is filled from the first user message.
    title text,

    -- The id of the message currently at the end of the thread.
    --
    -- Stored rather than computed as "the newest message" because messages form a
    -- tree, not a list: editing an earlier message creates a new branch, and only
    -- the head of the active branch is the conversation's current position.
    head_message_id uuid,

    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- The thread list is always "this user's conversations, most recently touched".
CREATE INDEX conversations_user_updated_idx ON conversations (user_id, updated_at DESC);

-- Messages ----------------------------------------------------------------

-- One row per message in a thread.
--
-- The table is a tree, not a list. parent_id makes an edit a branch: correcting a
-- message inserts a new one beside it and points superseded_message_id at what it
-- replaced, so the original stays readable and the conversation's active path moves
-- to the correction. Overwriting the row instead would destroy the only record of
-- what the assistant was originally asked, which is the thing worth being able to
-- look back at.
CREATE TABLE messages (
    id uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The message this one answers or revises. NULL for the opening message.
    parent_id uuid REFERENCES messages (id) ON DELETE CASCADE,

    -- The message this one replaced, when it is an edit. Set on the replacement,
    -- pointing at the original.
    superseded_message_id uuid REFERENCES messages (id) ON DELETE SET NULL,

    role text NOT NULL,

    -- The message text. For an assistant turn this is the reply, and for a planned
    -- turn it is the plan rendered as text, because a caller that cannot parse the
    -- plan column can still show something.
    content text NOT NULL,

    -- The model's reasoning, when it exposed any.
    --
    -- Nullable and expected to stay null: a non-thinking model reports none, and
    -- inventing a substitute would present a post-hoc rationalisation as if it were
    -- the model's own thinking.
    reasoning text,

    -- Set when this turn produced a run, so the plan and its approval remain
    -- reachable from the transcript.
    run_id uuid REFERENCES agent_runs (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL,

    -- Roles are a closed vocabulary: anything else is a bug rather than a new kind
    -- of turn. Checked in the schema so an unhandled role cannot reach a renderer
    -- that assumes it knows how to draw the message.
    CONSTRAINT messages_role_check CHECK (role IN ('user', 'assistant')),

    -- A message cannot be its own parent, which is the one cycle a chain walk
    -- cannot otherwise detect before recursing until the stack gives out.
    CONSTRAINT messages_not_own_parent_check CHECK (parent_id IS NULL OR parent_id <> id),

    -- An edit points at a different message, for the same reason.
    CONSTRAINT messages_not_supersede_self_check
        CHECK (superseded_message_id IS NULL OR superseded_message_id <> id)
);

-- Walking a conversation is always "every message of this conversation, oldest
-- first", so the pair is the index rather than conversation_id alone.
CREATE INDEX messages_conversation_created_idx ON messages (conversation_id, created_at);

-- Finding the children of a branch point is a lookup by parent, which the index
-- above does not serve: conversation_id is not the leading column here.
CREATE INDEX messages_parent_idx ON messages (parent_id);

-- A run belongs to at most one assistant message.
CREATE UNIQUE INDEX messages_run_key ON messages (run_id) WHERE run_id IS NOT NULL;

-- The head pointer ------------------------------------------------------------

-- Added after messages exists, because it references it.
--
-- SET NULL rather than CASCADE because the head can only be deleted with the
-- conversation that owns it, and cascading here would turn one conversation's
-- delete into a delete of messages in another.
ALTER TABLE conversations
    ADD CONSTRAINT conversations_head_message_fkey
    FOREIGN KEY (head_message_id) REFERENCES messages (id) ON DELETE SET NULL;
