-- 055: shadow evaluation and rollout gates (M4-06)
--
-- rollouts: one proposed configuration change. candidate_snapshot_id is a
-- config snapshot that is not the production default (the implementation-
-- agent snapshot is the rollout unit, M4-05 spec "Rollout unit");
-- baseline_snapshot_id is the production default it would replace.
-- decision_report holds the internal/rollout.Evaluate decision with the raw
-- counts and intervals it was computed from.
--
-- rollout_overrides: audit of every configuration-default change that was
-- applied without an approved rollout (an admin override with a recorded
-- reason) and of every approve/block decision that overrode the evaluator.
-- Snapshot ids are kept as plain text: the audit outlives pruned snapshots.
--
-- products.allow_replay: opt-in (default false) for re-submitting the
-- product's recent completed work items against a scratch product with a
-- candidate configuration (shadow replay). products.benchmark_scratch marks
-- the scratch products shadow runs may target (default false).
--
-- work_items shadow columns: replay marks a replay item (pr-coordinator never
-- opens a PR for it), replay_of is the production item it re-submits,
-- shadow_rollout_id the rollout it was run for, requested_config_snapshot_id
-- the snapshot the shadow intake asked for (AE_CONFIG_SNAPSHOT_ID).
--

CREATE TABLE IF NOT EXISTS rollouts (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_snapshot_id TEXT,
    baseline_snapshot_id  TEXT,
    scope                 TEXT        NOT NULL DEFAULT 'global',
    product_id            UUID,
    status                TEXT        NOT NULL DEFAULT 'proposed',
    created_by            TEXT        NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at            TIMESTAMPTZ,
    decided_by            TEXT,
    decision_report       JSONB,
    notes                 TEXT        NOT NULL DEFAULT ''
);

ALTER TABLE rollouts DROP CONSTRAINT IF EXISTS rollouts_candidate_snapshot_id_fkey;
ALTER TABLE rollouts ADD CONSTRAINT rollouts_candidate_snapshot_id_fkey
    FOREIGN KEY (candidate_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;
ALTER TABLE rollouts DROP CONSTRAINT IF EXISTS rollouts_baseline_snapshot_id_fkey;
ALTER TABLE rollouts ADD CONSTRAINT rollouts_baseline_snapshot_id_fkey
    FOREIGN KEY (baseline_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;
ALTER TABLE rollouts DROP CONSTRAINT IF EXISTS rollouts_product_id_fkey;
ALTER TABLE rollouts ADD CONSTRAINT rollouts_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE;
ALTER TABLE rollouts DROP CONSTRAINT IF EXISTS rollouts_status_check;
ALTER TABLE rollouts ADD CONSTRAINT rollouts_status_check
    CHECK (status IN ('proposed', 'shadow', 'evaluating', 'approved', 'blocked', 'rolled_back'));
ALTER TABLE rollouts DROP CONSTRAINT IF EXISTS rollouts_scope_check;
ALTER TABLE rollouts ADD CONSTRAINT rollouts_scope_check
    CHECK ((scope = 'global' AND product_id IS NULL) OR (scope = 'product' AND product_id IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_rollouts_candidate_status
    ON rollouts (candidate_snapshot_id, status);
CREATE INDEX IF NOT EXISTS idx_rollouts_created_at
    ON rollouts (created_at DESC);

CREATE TABLE IF NOT EXISTS rollout_overrides (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id         UUID,
    action             TEXT        NOT NULL,
    scope              TEXT        NOT NULL DEFAULT 'global',
    product_id         UUID,
    before_snapshot_id TEXT,
    after_snapshot_id  TEXT,
    override_reason    TEXT        NOT NULL,
    actor              TEXT        NOT NULL DEFAULT '',
    details            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT rollout_overrides_reason_check CHECK (length(btrim(override_reason)) > 0)
);

ALTER TABLE rollout_overrides DROP CONSTRAINT IF EXISTS rollout_overrides_rollout_id_fkey;
ALTER TABLE rollout_overrides ADD CONSTRAINT rollout_overrides_rollout_id_fkey
    FOREIGN KEY (rollout_id) REFERENCES rollouts(id) ON DELETE SET NULL;
ALTER TABLE rollout_overrides DROP CONSTRAINT IF EXISTS rollout_overrides_product_id_fkey;
ALTER TABLE rollout_overrides ADD CONSTRAINT rollout_overrides_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_rollout_overrides_created_at
    ON rollout_overrides (created_at DESC);

ALTER TABLE products ADD COLUMN IF NOT EXISTS allow_replay BOOLEAN NOT NULL DEFAULT false;
-- benchmark_scratch: the product is a scratch target for shadow runs (its
-- repositories are forks that may receive agent branches and PRs). Only a
-- scratch product may run a requested configuration snapshot or receive a
-- replay. Admin-set; default false.
ALTER TABLE products ADD COLUMN IF NOT EXISTS benchmark_scratch BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE work_items ADD COLUMN IF NOT EXISTS replay BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS replay_of UUID;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS shadow_rollout_id UUID;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS requested_config_snapshot_id TEXT;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_replay_of_fkey;
ALTER TABLE work_items ADD CONSTRAINT work_items_replay_of_fkey
    FOREIGN KEY (replay_of) REFERENCES work_items(id) ON DELETE SET NULL;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_shadow_rollout_id_fkey;
ALTER TABLE work_items ADD CONSTRAINT work_items_shadow_rollout_id_fkey
    FOREIGN KEY (shadow_rollout_id) REFERENCES rollouts(id) ON DELETE SET NULL;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_requested_config_snapshot_id_fkey;
ALTER TABLE work_items ADD CONSTRAINT work_items_requested_config_snapshot_id_fkey
    FOREIGN KEY (requested_config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_work_items_shadow_rollout
    ON work_items (shadow_rollout_id) WHERE shadow_rollout_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_work_items_replay_of
    ON work_items (replay_of) WHERE replay_of IS NOT NULL;
