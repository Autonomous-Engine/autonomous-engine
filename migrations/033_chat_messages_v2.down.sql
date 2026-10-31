ALTER TABLE chat_messages DROP COLUMN IF EXISTS proposal;
ALTER TABLE chat_messages ALTER COLUMN work_item_id SET NOT NULL;
ALTER TABLE chat_messages DROP CONSTRAINT IF EXISTS fk_chat_messages_session;
DELETE FROM chat_sessions WHERE title = 'Migrated session';
