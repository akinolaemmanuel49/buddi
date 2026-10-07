-- Removes the pending-question marker.
--
-- Only the column goes: the messages and conversations added in 000007 are still
-- needed, and dropping them here would take the transcript with them.

ALTER TABLE messages
    DROP COLUMN IF EXISTS clarification_request;