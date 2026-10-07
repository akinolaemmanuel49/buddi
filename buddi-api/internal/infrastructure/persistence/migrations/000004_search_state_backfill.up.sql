-- Derive notes.search_state from the chunks that already exist.
--
-- Split from migration 000003, which added the columns, because a label is only
-- true if it describes something that exists. Migration 000003's column default is
-- 'pending', which is right for a note written from now on but wrong for every note
-- indexed before it: those are searchable, and labelling them pending would have the
-- API tell the user their note is not searchable when it is, while also queueing
-- every historical note for re-embedding.
--
-- Deriving the initial value from the chunks is the one place inference is right.
-- The column did not exist yet, so the chunks are the only evidence there is;
-- afterwards the column is authoritative and the two are kept in step by the note
-- service.
--
-- It is a separate migration rather than a trailing statement in 000003 because
-- 000003 has already run against the development database. Rewriting an applied
-- migration means the file on disk and the schema in the database silently disagree,
-- and the version number cannot tell you which one you are looking at.
--
-- The label is computed once in a CTE because two facts have to agree: what the note
-- is called, and when it was last proven searchable. Deriving them separately is how a
-- note ends up marked pending with an indexed_at stamp, or indexed with no timestamp,
-- and the next reader has no way to tell which statement is lying.

WITH labelled AS (
    SELECT
        n.id,
        CASE
            -- Indexed means "chunks exist and search can serve them". The embedding
            -- clause matters because note_chunks.embedding is nullable and a NULL is
            -- invisible to the HNSW index: a note whose only chunks have no vector
            -- looks indexed on disk and never matches a query. Require at least one
            -- embedded chunk and no unembedded ones, so a half-written note is queued
            -- for re-indexing rather than trusted.
            WHEN EXISTS (
                     SELECT 1 FROM note_chunks c
                     WHERE c.note_id = n.id AND c.embedding IS NOT NULL
                 )
             AND NOT EXISTS (
                     SELECT 1 FROM note_chunks c
                     WHERE c.note_id = n.id AND c.embedding IS NULL
                 )
                THEN 'indexed'
            -- Everything else stays pending, which is the honest state for it: the note
            -- is written but not yet searchable. A note with no chunks at all has
            -- nothing to search with, and a note with unembedded chunks has something
            -- search cannot use.
            ELSE 'pending'
        END AS state,
        -- The newest chunk is the closest thing to when the note became searchable.
        -- ReplaceChunks writes every chunk of a note in one transaction with one
        -- timestamp, so this does not misrepresent a partial write.
        (SELECT max(c.created_at) FROM note_chunks c WHERE c.note_id = n.id) AS last_chunk
    FROM notes n
)

UPDATE notes n
SET search_state = l.state,
    -- Stamped only when the note is actually labelled indexed. A pending note with a
    -- timestamp would be a second, contradictory answer to the same question.
    indexed_at = CASE WHEN l.state = 'indexed' THEN l.last_chunk ELSE NULL END
FROM labelled l
WHERE l.id = n.id;
