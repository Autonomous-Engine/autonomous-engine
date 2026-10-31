CREATE TABLE conversation_snapshots (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id  UUID NOT NULL REFERENCES work_items(id),
    task_id       UUID,
    agent_type    TEXT NOT NULL,
    snapshot_seq  INT NOT NULL DEFAULT 0,
    messages      JSONB NOT NULL,
    summary       TEXT NOT NULL,
    turn_count    INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_conv_snapshots_work_item ON conversation_snapshots(work_item_id);
CREATE INDEX idx_conv_snapshots_task ON conversation_snapshots(task_id) WHERE task_id IS NOT NULL;
