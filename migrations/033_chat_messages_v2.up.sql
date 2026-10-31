INSERT INTO chat_sessions (id, user_id, title, active_work_item_id, created_at, updated_at)
SELECT DISTINCT ON (session_id)
    session_id,
    'admin',
    'Migrated session',
    work_item_id,
    MIN(created_at) OVER (PARTITION BY session_id),
    MAX(created_at) OVER (PARTITION BY session_id)
FROM chat_messages
ON CONFLICT (id) DO NOTHING;

ALTER TABLE chat_messages
    ADD CONSTRAINT fk_chat_messages_session FOREIGN KEY (session_id) REFERENCES chat_sessions(id);

ALTER TABLE chat_messages
    ALTER COLUMN work_item_id DROP NOT NULL;

ALTER TABLE chat_messages
    ADD COLUMN proposal JSONB;
