-- 052: implementation_runs / implementation_run_events (M3-01/02)
-- Persisted state of one implementation-agent attempt at a subtask. The
-- state is advanced only by internal/controller.Derive from tool evidence
-- (write_file/edit_file paths, run_command exit codes, git_diff/read_file)
-- and, for final_verified, by the final gate's evidence outcome. Every tool
-- callback appends a 'tool' event; every state change appends a
-- 'transition' event and updates implementation_runs.state in one tx.
--
-- work_item_id is the parent work item (the outbox aggregate); task_id is
-- the subtask id. A redelivered task starts a new row with attempt+1.
-- detail carries per-run controller data (red/green evidence pointers, and
-- later the M3-06 budget / M3-07 failure-fingerprint state).

CREATE TABLE IF NOT EXISTS implementation_runs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id   UUID        NOT NULL,
    task_id        TEXT        NOT NULL,
    attempt        INT         NOT NULL,
    state          TEXT        NOT NULL DEFAULT 'inspect',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_event_seq BIGINT      NOT NULL DEFAULT 0,
    detail         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT implementation_runs_state_check
        CHECK (state IN ('inspect', 'test_authored', 'red_observed', 'implement',
                         'green_observed', 'diff_reviewed', 'final_verified',
                         'blocked', 'failed')),
    CONSTRAINT implementation_runs_task_attempt_key UNIQUE (task_id, attempt),
    CONSTRAINT implementation_runs_work_item_id_fkey
        FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_implementation_runs_work_item
    ON implementation_runs (work_item_id, task_id, attempt DESC);

CREATE TABLE IF NOT EXISTS implementation_run_events (
    run_id       UUID        NOT NULL,
    seq          BIGINT      NOT NULL,
    kind         TEXT        NOT NULL,
    tool_call_id TEXT,
    tool         TEXT,
    exit_code    INT,
    path         TEXT,
    command      TEXT,
    from_state   TEXT,
    to_state     TEXT,
    note         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT implementation_run_events_pkey PRIMARY KEY (run_id, seq),
    CONSTRAINT implementation_run_events_kind_check
        CHECK (kind IN ('tool', 'transition', 'note')),
    CONSTRAINT implementation_run_events_run_id_fkey
        FOREIGN KEY (run_id) REFERENCES implementation_runs(id) ON DELETE CASCADE
);

-- console.stream now carries the run's phase and run_command's exit code;
-- dashboard-backend persists both with the tool event (NULL for events from
-- agents without a controller and for rows written before this migration).
ALTER TABLE tool_events ADD COLUMN IF NOT EXISTS phase TEXT NULL;
ALTER TABLE tool_events ADD COLUMN IF NOT EXISTS exit_code INT NULL;
