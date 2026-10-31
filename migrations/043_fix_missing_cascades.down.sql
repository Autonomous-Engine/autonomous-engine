-- Revert to bare REFERENCES (no ON DELETE behavior).

ALTER TABLE pending_pr_changes DROP CONSTRAINT IF EXISTS pending_pr_changes_work_item_id_fkey;
ALTER TABLE pending_pr_changes ADD CONSTRAINT pending_pr_changes_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id);

ALTER TABLE conversation_snapshots DROP CONSTRAINT IF EXISTS conversation_snapshots_work_item_id_fkey;
ALTER TABLE conversation_snapshots ADD CONSTRAINT conversation_snapshots_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id);

ALTER TABLE chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_active_work_item_id_fkey;
ALTER TABLE chat_sessions ADD CONSTRAINT chat_sessions_active_work_item_id_fkey
    FOREIGN KEY (active_work_item_id) REFERENCES work_items(id);

ALTER TABLE chat_session_work_items DROP CONSTRAINT IF EXISTS chat_session_work_items_work_item_id_fkey;
ALTER TABLE chat_session_work_items ADD CONSTRAINT chat_session_work_items_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id);

ALTER TABLE chat_session_work_items DROP CONSTRAINT IF EXISTS chat_session_work_items_session_id_fkey;
ALTER TABLE chat_session_work_items ADD CONSTRAINT chat_session_work_items_session_id_fkey
    FOREIGN KEY (session_id) REFERENCES chat_sessions(id);
