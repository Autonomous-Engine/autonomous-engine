-- R2: research rounds persisted with their dispatched task set.
--
-- research-coordinator kept a research round only in memory, so a restart
-- judged convergence against the results seen since the process started
-- (one result looked like "all done", B2), and a retry after a block threw
-- away every completed task and paid for it again. A round now records the
-- PRD hash it was dispatched for and every task in its set; convergence is
-- judged against these rows, and a retry with the same PRD hash reuses the
-- ok results of earlier rounds and dispatches only the failed or missing
-- research types.

CREATE TABLE IF NOT EXISTS research_rounds (
    id                 UUID PRIMARY KEY,
    work_item_id       UUID NOT NULL,
    prd_hash           TEXT NOT NULL,
    work_type          TEXT NOT NULL DEFAULT '',
    product_id         TEXT NOT NULL DEFAULT '',
    prd_content        TEXT NOT NULL DEFAULT '',
    aggregate_id       UUID,
    assignment_id      UUID,
    repo_list          JSONB NOT NULL DEFAULT '[]'::jsonb,
    status             TEXT NOT NULL DEFAULT 'open',
    claimed_by_task_id TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at          TIMESTAMPTZ,
    CONSTRAINT research_rounds_work_item_id_fkey
        FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE,
    CONSTRAINT research_rounds_status_check
        CHECK (status IN ('open', 'synthesizing', 'completed', 'blocked', 'superseded'))
);

CREATE INDEX IF NOT EXISTS research_rounds_work_item_idx
    ON research_rounds (work_item_id, created_at DESC);

CREATE TABLE IF NOT EXISTS research_round_tasks (
    task_id             TEXT PRIMARY KEY,
    round_id            UUID NOT NULL,
    research_type       TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    result              JSONB,
    reused_from_task_id TEXT,
    followup            BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ,
    CONSTRAINT research_round_tasks_round_id_fkey
        FOREIGN KEY (round_id) REFERENCES research_rounds(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS research_round_tasks_round_idx
    ON research_round_tasks (round_id);
