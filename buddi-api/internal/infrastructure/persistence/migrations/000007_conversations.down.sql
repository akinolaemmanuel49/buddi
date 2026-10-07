-- Drops the chat transcript.
--
-- messages first: conversations.head_message_id references it, so removing
-- conversations first would leave that constraint pointing at a dropped table.

DROP TABLE IF EXISTS messages;

DROP TABLE IF EXISTS conversations;
