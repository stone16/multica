-- Immutable platform routing for channel-ingested user messages. The mutable
-- channel_chat_session_binding reply target can advance before an agent creates
-- an issue, so completion notifications must correlate through the task-owned
-- input message instead.
ALTER TABLE chat_message
    ADD COLUMN channel_message_id TEXT,
    ADD COLUMN channel_thread_id TEXT;
