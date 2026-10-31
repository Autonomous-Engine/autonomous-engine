-- Fix bare REFERENCES that lack ON DELETE behavior.
-- Without CASCADE/SET NULL, deleting a parent row (work_item, repository)
-- fails with a FK violation if any child rows exist.

-- pending_pr_changes.work_item_id (029)
ALTER TABLE pending_pr_changes DROP CONSTRAINT IF EXISTS pending_pr_changes_work_item_id_fkey;
ALTER TABLE pending_pr_changes ADD CONSTRAINT pending_pr_changes_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

-- conversation_snapshots.work_item_id (030)
ALTER TABLE conversation_snapshots DROP CONSTRAINT IF EXISTS conversation_snapshots_work_item_id_fkey;
ALTER TABLE conversation_snapshots ADD CONSTRAINT conversation_snapshots_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

-- chat_sessions.active_work_item_id (032)
ALTER TABLE chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_active_work_item_id_fkey;
ALTER TABLE chat_sessions ADD CONSTRAINT chat_sessions_active_work_item_id_fkey
    FOREIGN KEY (active_work_item_id) REFERENCES work_items(id) ON DELETE SET NULL;

-- chat_session_work_items.work_item_id (032)
ALTER TABLE chat_session_work_items DROP CONSTRAINT IF EXISTS chat_session_work_items_work_item_id_fkey;
ALTER TABLE chat_session_work_items ADD CONSTRAINT chat_session_work_items_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

-- chat_session_work_items.session_id (032) — cascade if session deleted
ALTER TABLE chat_session_work_items DROP CONSTRAINT IF EXISTS chat_session_work_items_session_id_fkey;
ALTER TABLE chat_session_work_items ADD CONSTRAINT chat_session_work_items_session_id_fkey
    FOREIGN KEY (session_id) REFERENCES chat_sessions(id) ON DELETE CASCADE;
