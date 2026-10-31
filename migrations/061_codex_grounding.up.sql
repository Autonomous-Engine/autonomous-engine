-- 061: codex_grounding_refusals, codex_gap_events, codex_gap_runs
-- Codex coherence, Part 3 (docs: codex coherence design): no planning or
-- implementation on an empty codex.
--
-- 1. codex_grounding_refusals records a stage that refused a work item
--    because the codex of its product has no module on its branch
--    (guards.CheckCodexGrounded). One row per work item: the stage, the
--    state the item was in, and the message that stage was handling, so
--    the item can be re-run exactly where it stopped. The refusal's
--    escalation prompt offers Re-index / Retry / Cancel. Re-index sets
--    reindex_requested_at (dashboard-backend) and asks knowledge-indexer
--    for a full index; status-orchestrator re-runs the item when a
--    codex.indexed event for the product arrives and the codex is grounded.
--    The row is the durable pending re-run: a restart loses nothing.
--    block_transition_id is the work_item_transitions row of the block the
--    refusal created: the refusal replays only while that block is still
--    the item's latest (prompt park/restore through awaiting_user does not
--    count), so a stale row cannot re-run an item blocked later for another
--    reason. index_posted_at is when knowledge-indexer accepted the
--    Re-index; NULL with reindex_requested_at set means it is still to be
--    posted (the indexer answered 409, busy with another index) and
--    status-orchestrator re-posts it.
-- 2. codex_gap_events records each codex.gap "empty codex lookup" event
--    (deduplicated on gap_id), and codex_gap_runs counts them per work item
--    and run: one research or implementation run starts at a transition
--    into that state that is not a prompt restore (run_started_at), and a
--    lookup counts for the run containing its own time (event_at). A run with
--    more than CODEX_GAP_THRESHOLD (default 5) empty lookups gets
--    ungrounded_at, and the verification stage then does not approve
--    automatically (evidence flag "ungrounded").

CREATE TABLE IF NOT EXISTS codex_grounding_refusals (
    work_item_id         UUID        NOT NULL,
    product_id           UUID        NOT NULL,
    branch               TEXT        NOT NULL DEFAULT '',
    index_branch         TEXT        NOT NULL DEFAULT '',
    stage                TEXT        NOT NULL,
    reason               TEXT        NOT NULL DEFAULT '',
    rerun_state          TEXT        NOT NULL,
    rerun_topic          TEXT        NOT NULL,
    rerun_key            TEXT        NOT NULL,
    rerun_aggregate_id   UUID        NOT NULL,
    rerun_payload        JSONB       NOT NULL,
    block_transition_id  UUID,
    reindex_requested_at TIMESTAMPTZ,
    reindex_requested_by TEXT,
    index_posted_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE codex_grounding_refusals DROP CONSTRAINT IF EXISTS codex_grounding_refusals_pkey;
ALTER TABLE codex_grounding_refusals ADD CONSTRAINT codex_grounding_refusals_pkey
    PRIMARY KEY (work_item_id);

ALTER TABLE codex_grounding_refusals DROP CONSTRAINT IF EXISTS codex_grounding_refusals_work_item_id_fkey;
ALTER TABLE codex_grounding_refusals ADD CONSTRAINT codex_grounding_refusals_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE codex_grounding_refusals DROP CONSTRAINT IF EXISTS codex_grounding_refusals_product_id_fkey;
ALTER TABLE codex_grounding_refusals ADD CONSTRAINT codex_grounding_refusals_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS codex_grounding_refusals_reindex_idx
    ON codex_grounding_refusals (product_id)
    WHERE reindex_requested_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS codex_grounding_refusals_unposted_idx
    ON codex_grounding_refusals (reindex_requested_at)
    WHERE reindex_requested_at IS NOT NULL AND index_posted_at IS NULL;

CREATE TABLE IF NOT EXISTS codex_gap_runs (
    work_item_id   UUID        NOT NULL,
    stage          TEXT        NOT NULL,
    run_started_at TIMESTAMPTZ NOT NULL,
    empty_lookups  INT         NOT NULL DEFAULT 0,
    ungrounded_at  TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE codex_gap_runs DROP CONSTRAINT IF EXISTS codex_gap_runs_pkey;
ALTER TABLE codex_gap_runs ADD CONSTRAINT codex_gap_runs_pkey
    PRIMARY KEY (work_item_id, stage, run_started_at);

ALTER TABLE codex_gap_runs DROP CONSTRAINT IF EXISTS codex_gap_runs_work_item_id_fkey;
ALTER TABLE codex_gap_runs ADD CONSTRAINT codex_gap_runs_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

CREATE TABLE IF NOT EXISTS codex_gap_events (
    gap_id         TEXT        NOT NULL,
    work_item_id   UUID        NOT NULL,
    stage          TEXT        NOT NULL,
    run_started_at TIMESTAMPTZ NOT NULL,
    codex_tool     TEXT        NOT NULL DEFAULT '',
    query          TEXT        NOT NULL DEFAULT '',
    fallback_tool  TEXT        NOT NULL DEFAULT '',
    found_via      TEXT        NOT NULL DEFAULT '',
    event_at       TIMESTAMPTZ,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE codex_gap_events DROP CONSTRAINT IF EXISTS codex_gap_events_pkey;
ALTER TABLE codex_gap_events ADD CONSTRAINT codex_gap_events_pkey
    PRIMARY KEY (gap_id);

ALTER TABLE codex_gap_events DROP CONSTRAINT IF EXISTS codex_gap_events_work_item_id_fkey;
ALTER TABLE codex_gap_events ADD CONSTRAINT codex_gap_events_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS codex_gap_events_work_item_idx
    ON codex_gap_events (work_item_id, stage, run_started_at);
