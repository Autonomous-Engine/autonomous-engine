-- 050: work_item_base_shas, base_sha_events
-- Repository snapshot pinning (roadmap 2.1, tickets M2-01/M2-02). Before this
-- no base commit was recorded before implementation: research read whatever
-- the shared clone's working tree held, implementation branched from the local
-- base-branch name, and the only base SHA was a merge-base computed after the
-- fact. product-agent now resolves origin/<base_branch> to a commit at intake
-- and pins it here; the planner copies the parent's pin to each repo-scoped
-- subtask.
--
-- One row per (work item, repository): a product work item can span several
-- repositories, each with its own base. No row means the item predates
-- pinning (legacy-unpinned); readers keep the old branch-name behaviour.
-- There is no backfill.

CREATE TABLE IF NOT EXISTS work_item_base_shas (
    work_item_id  UUID        NOT NULL,
    repository_id UUID        NOT NULL,
    base_ref      TEXT        NOT NULL,
    base_sha      TEXT        NOT NULL,
    resolved_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE work_item_base_shas DROP CONSTRAINT IF EXISTS work_item_base_shas_pkey;
ALTER TABLE work_item_base_shas ADD CONSTRAINT work_item_base_shas_pkey
    PRIMARY KEY (work_item_id, repository_id);

-- A full, lower-case hex commit id: an abbreviated or symbolic value is not a
-- pin.
ALTER TABLE work_item_base_shas DROP CONSTRAINT IF EXISTS work_item_base_shas_base_sha_check;
ALTER TABLE work_item_base_shas ADD CONSTRAINT work_item_base_shas_base_sha_check
    CHECK (base_sha ~ '^[0-9a-f]{40}$');

ALTER TABLE work_item_base_shas DROP CONSTRAINT IF EXISTS work_item_base_shas_work_item_id_fkey;
ALTER TABLE work_item_base_shas ADD CONSTRAINT work_item_base_shas_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE work_item_base_shas DROP CONSTRAINT IF EXISTS work_item_base_shas_repository_id_fkey;
ALTER TABLE work_item_base_shas ADD CONSTRAINT work_item_base_shas_repository_id_fkey
    FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE;

-- The audit trail of every pin: written when a pin is created (resolved), when
-- a later comparison finds the base has moved (drift_detected) and, from
-- M2-03, when a pin is replaced (repinned). Drift is never silent.
CREATE TABLE IF NOT EXISTS base_sha_events (
    id            UUID        NOT NULL DEFAULT gen_random_uuid(),
    work_item_id  UUID        NOT NULL,
    repository_id UUID        NOT NULL,
    kind          TEXT        NOT NULL,
    old_sha       TEXT        NOT NULL DEFAULT '',
    new_sha       TEXT        NOT NULL DEFAULT '',
    detail        TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE base_sha_events DROP CONSTRAINT IF EXISTS base_sha_events_pkey;
ALTER TABLE base_sha_events ADD CONSTRAINT base_sha_events_pkey PRIMARY KEY (id);

ALTER TABLE base_sha_events DROP CONSTRAINT IF EXISTS base_sha_events_kind_check;
ALTER TABLE base_sha_events ADD CONSTRAINT base_sha_events_kind_check
    CHECK (kind IN ('resolved', 'drift_detected', 'repinned'));

ALTER TABLE base_sha_events DROP CONSTRAINT IF EXISTS base_sha_events_work_item_id_fkey;
ALTER TABLE base_sha_events ADD CONSTRAINT base_sha_events_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE base_sha_events DROP CONSTRAINT IF EXISTS base_sha_events_repository_id_fkey;
ALTER TABLE base_sha_events ADD CONSTRAINT base_sha_events_repository_id_fkey
    FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_base_sha_events_work_item_created
    ON base_sha_events (work_item_id, created_at);
