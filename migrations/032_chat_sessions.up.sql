CREATE TABLE chat_sessions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             TEXT NOT NULL,
    title               TEXT,
    page_context        JSONB,
    active_work_item_id UUID REFERENCES work_items(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chat_sessions_user ON chat_sessions(user_id, created_at DESC);
CREATE INDEX idx_chat_sessions_work_item ON chat_sessions(active_work_item_id) WHERE active_work_item_id IS NOT NULL;

CREATE TABLE chat_session_work_items (
    session_id    UUID NOT NULL REFERENCES chat_sessions(id),
    work_item_id  UUID NOT NULL REFERENCES work_items(id),
    relationship  TEXT NOT NULL CHECK (relationship IN ('created', 'modified', 'referenced')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (session_id, work_item_id)
);
