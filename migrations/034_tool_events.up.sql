CREATE TABLE tool_events (
    id            BIGSERIAL PRIMARY KEY,
    work_item_id  UUID NOT NULL,
    task_id       TEXT NOT NULL DEFAULT '',
    agent_type    TEXT NOT NULL DEFAULT '',
    tool          TEXT NOT NULL,
    arguments     TEXT NOT NULL DEFAULT '',
    result        TEXT NOT NULL DEFAULT '',
    is_error      BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tool_events_work_item ON tool_events(work_item_id);
CREATE INDEX idx_tool_events_work_item_created ON tool_events(work_item_id, created_at);
