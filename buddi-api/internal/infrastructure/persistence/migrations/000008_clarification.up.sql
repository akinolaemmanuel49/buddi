-- Records that an assistant turn is a pending question rather than a reply.
--
-- A clarification is a question the user has to answer before anything can happen,
-- and the answer arrives as the next message in a later request. The chat service is
-- stateless per request, so "is a question still outstanding?" has to be readable from
-- the transcript rather than held in memory that dies with the connection.
--
-- The column holds the original request rather than the question, because the question
-- is already the message content and what is actually needed to resume is the wording
-- the planner was first given. The user's reply alone ("Tuesday at 4pm") names no
-- dentist, so re-planning from it alone would lose the subject.
--
-- Nullable because almost every message is not a question.

ALTER TABLE messages
    ADD COLUMN clarification_request text;

COMMENT ON COLUMN messages.clarification_request IS
    'Set on an assistant message that asked a question instead of answering. Holds the original request, so the next user reply can be re-planned with the subject still attached. NULL for every other message.';