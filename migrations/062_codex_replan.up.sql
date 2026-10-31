-- 062: codex_replan_pending, work_item_codex_events
-- Codex coherence, Part 1 (docs: codex coherence design): re-plan in-flight
-- work when the codex changes.
--
-- 1. codex_replan_pending is the durable debounce of codex.indexed. One row
--    per (product_id, branch) with a pending re-plan sweep. Each event
--    pushes due_at to now() + CODEX_REPLAN_DEBOUNCE, so a burst collapses
--    into one sweep after the last event; status-orchestrator's ticker
--    claims due rows with FOR UPDATE SKIP LOCKED (claim_token + a lease in
--    due_at), so replicas never sweep the same row twice and a crash or a
--    restart during the debounce or the sweep only delays it.
-- 2. work_item_codex_events is the per-item record of what the codex
--    re-plan did: replanned (the item was sent back to planning), escalated
--    (the automatic re-plan cap or the budget was reached), noted (the codex
--    changed since planning but the item is past planning, e.g. in
--    verification or with an open PR, and was left alone), continued (the
--    operator chose "Continue as is"). It is the dashboard's activity entry
--    and "codex changed since planning" flag, the loop guard's durable count
--    (automatic replanned rows), and the version every later sweep starts
--    from (MAX(to_version)). created_at is clock_timestamp() so the planner
--    can tell a plan started before a re-plan from one started after it.

CREATE TABLE IF NOT EXISTS codex_replan_pending (
    product_id       UUID        NOT NULL,
    branch           TEXT        NOT NULL,
    due_at           TIMESTAMPTZ NOT NULL,
    first_event_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_event_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    latest_version   INT         NOT NULL DEFAULT 0,
    sections_changed TEXT[]      NOT NULL DEFAULT '{}',
    event_count      INT         NOT NULL DEFAULT 1,
    claim_token      UUID        NULL
);

ALTER TABLE codex_replan_pending DROP CONSTRAINT IF EXISTS codex_replan_pending_pkey;
ALTER TABLE codex_replan_pending ADD CONSTRAINT codex_replan_pending_pkey
    PRIMARY KEY (product_id, branch);

ALTER TABLE codex_replan_pending DROP CONSTRAINT IF EXISTS codex_replan_pending_product_id_fkey;
ALTER TABLE codex_replan_pending ADD CONSTRAINT codex_replan_pending_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE;

ALTER TABLE codex_replan_pending DROP CONSTRAINT IF EXISTS codex_replan_pending_branch_check;
ALTER TABLE codex_replan_pending ADD CONSTRAINT codex_replan_pending_branch_check
    CHECK (branch <> '');

CREATE INDEX IF NOT EXISTS codex_replan_pending_due_idx
    ON codex_replan_pending (due_at);

CREATE TABLE IF NOT EXISTS work_item_codex_events (
    id           UUID        NOT NULL DEFAULT gen_random_uuid(),
    work_item_id UUID        NOT NULL,
    product_id   UUID        NOT NULL,
    branch       TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    automatic    BOOLEAN     NOT NULL DEFAULT true,
    from_version INT         NOT NULL,
    to_version   INT         NOT NULL,
    modules      TEXT[]      NOT NULL DEFAULT '{}',
    sections     TEXT[]      NOT NULL DEFAULT '{}',
    item_state   TEXT        NOT NULL DEFAULT '',
    reason       TEXT        NOT NULL DEFAULT '',
    -- why an escalation or a note was raised: cap, budget, no_brief,
    -- verified_work, refused ('' for the other kinds)
    cause        TEXT        NOT NULL DEFAULT '',
    prompt_id    UUID        NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_pkey;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_pkey
    PRIMARY KEY (id);

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_work_item_id_fkey;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_product_id_fkey;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE;

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_prompt_id_fkey;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_prompt_id_fkey
    FOREIGN KEY (prompt_id) REFERENCES user_interactions(id) ON DELETE SET NULL;

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_kind_check;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_kind_check
    CHECK (kind IN ('replanned', 'escalated', 'noted', 'continued'));

ALTER TABLE work_item_codex_events DROP CONSTRAINT IF EXISTS work_item_codex_events_versions_check;
ALTER TABLE work_item_codex_events ADD CONSTRAINT work_item_codex_events_versions_check
    CHECK (from_version >= 0 AND to_version >= from_version);

CREATE INDEX IF NOT EXISTS work_item_codex_events_item_idx
    ON work_item_codex_events (work_item_id, created_at);

CREATE INDEX IF NOT EXISTS work_item_codex_events_prompt_idx
    ON work_item_codex_events (prompt_id) WHERE prompt_id IS NOT NULL;
