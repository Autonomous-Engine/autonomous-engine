-- 053: configuration snapshots, outcome records, model prices (M4-05)
--
-- config_snapshots: an immutable, content-addressed capture of everything
-- that shapes an agent run (internal/configsnap). id is the lowercase hex
-- sha256 of the canonical JSON of `document`; a row is never updated after
-- it is inserted (INSERT ... ON CONFLICT (id) DO NOTHING). The scalar
-- columns are copies of document fields for filtering.
--
-- Linkage columns (all nullable, ON DELETE SET NULL): work_items (intake and
-- each implementation attempt), implementation_runs (per attempt),
-- implementation_verifications (the evidence row), inference_logs (per
-- model call, with role and agent_type for cost tagging).
--
-- work_item_outcomes: one row per top-level work item, upserted by
-- status-orchestrator in the transaction of the state change that produced
-- the fact (internal/outcomes). It stores no user text.
--
-- model_prices: USD per 1M input/output tokens, effective from a timestamp.
-- Seeded with placeholder rows whose prices are NULL; cost stays NULL (never
-- zero) until an admin fills them in. inference_log_costs is the one place
-- the price join is written.
--

CREATE TABLE IF NOT EXISTS config_snapshots (
    id                  TEXT        PRIMARY KEY,
    schema_version      INT         NOT NULL,
    agent_definition_id UUID,
    agent_type          TEXT        NOT NULL DEFAULT '',
    provider            TEXT        NOT NULL DEFAULT '',
    model_id            TEXT        NOT NULL DEFAULT '',
    document            JSONB       NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT config_snapshots_id_check CHECK (id ~ '^[0-9a-f]{64}$')
);

CREATE INDEX IF NOT EXISTS idx_config_snapshots_created_at
    ON config_snapshots (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_config_snapshots_definition
    ON config_snapshots (agent_definition_id, agent_type);

-- work_items: set at intake and again at every implementation attempt.
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS config_snapshot_id TEXT;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_config_snapshot_id_fkey;
ALTER TABLE work_items ADD CONSTRAINT work_items_config_snapshot_id_fkey
    FOREIGN KEY (config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_work_items_config_snapshot
    ON work_items (config_snapshot_id) WHERE config_snapshot_id IS NOT NULL;

-- implementation_runs: the attempt's snapshot.
ALTER TABLE implementation_runs ADD COLUMN IF NOT EXISTS config_snapshot_id TEXT;
ALTER TABLE implementation_runs DROP CONSTRAINT IF EXISTS implementation_runs_config_snapshot_id_fkey;
ALTER TABLE implementation_runs ADD CONSTRAINT implementation_runs_config_snapshot_id_fkey
    FOREIGN KEY (config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;

-- implementation_verifications: the snapshot the evidence was produced under.
ALTER TABLE implementation_verifications ADD COLUMN IF NOT EXISTS config_snapshot_id TEXT;
ALTER TABLE implementation_verifications DROP CONSTRAINT IF EXISTS implementation_verifications_config_snapshot_id_fkey;
ALTER TABLE implementation_verifications ADD CONSTRAINT implementation_verifications_config_snapshot_id_fkey
    FOREIGN KEY (config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;

-- inference_logs: snapshot + cost tagging (role, agent_type).
ALTER TABLE inference_logs ADD COLUMN IF NOT EXISTS config_snapshot_id TEXT;
ALTER TABLE inference_logs ADD COLUMN IF NOT EXISTS role TEXT;
ALTER TABLE inference_logs ADD COLUMN IF NOT EXISTS agent_type TEXT;
ALTER TABLE inference_logs DROP CONSTRAINT IF EXISTS inference_logs_config_snapshot_id_fkey;
ALTER TABLE inference_logs ADD CONSTRAINT inference_logs_config_snapshot_id_fkey
    FOREIGN KEY (config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL;
ALTER TABLE inference_logs DROP CONSTRAINT IF EXISTS inference_logs_role_check;
ALTER TABLE inference_logs ADD CONSTRAINT inference_logs_role_check
    CHECK (role IS NULL OR role IN ('author', 'verifier', 'reviewer', 'planner', 'research', 'qa', 'other'));
CREATE INDEX IF NOT EXISTS idx_inference_logs_work_item
    ON inference_logs (work_item_id);
CREATE INDEX IF NOT EXISTS idx_inference_logs_config_snapshot
    ON inference_logs (config_snapshot_id) WHERE config_snapshot_id IS NOT NULL;

-- model_prices
CREATE TABLE IF NOT EXISTS model_prices (
    id             UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    provider       TEXT          NOT NULL,
    model_id       TEXT          NOT NULL,
    input_per_1m   NUMERIC(14,6),
    output_per_1m  NUMERIC(14,6),
    effective_from TIMESTAMPTZ   NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    updated_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT model_prices_key UNIQUE (provider, model_id, effective_from),
    CONSTRAINT model_prices_nonnegative CHECK (
        (input_per_1m IS NULL OR input_per_1m >= 0) AND (output_per_1m IS NULL OR output_per_1m >= 0))
);

-- Placeholder rows (NULL prices) for every model the system has used or is
-- configured to use, so the admin editor lists them. inference_logs rows
-- written before 053 carry "provider/model" in model_provider and an empty
-- or duplicated model_id; the same normalisation as inference_log_costs
-- applies.
INSERT INTO model_prices (provider, model_id)
SELECT DISTINCT provider, model_id FROM (
    SELECT provider, model_id FROM agent_definitions WHERE provider <> '' AND model_id <> ''
    UNION
    SELECT
        CASE WHEN position('/' IN model_provider) > 0 THEN split_part(model_provider, '/', 1) ELSE model_provider END,
        CASE WHEN model_id <> '' AND model_id <> model_provider THEN model_id
             WHEN position('/' IN model_provider) > 0 THEN substr(model_provider, position('/' IN model_provider) + 1)
             ELSE model_id END
    FROM inference_logs
) m
WHERE provider <> '' AND model_id <> ''
ON CONFLICT (provider, model_id, effective_from) DO NOTHING;

-- Per-call cost. NULL when the call's model has no price row effective at
-- the call time, or the effective row's prices are NULL (placeholder).
CREATE OR REPLACE VIEW inference_log_costs AS
SELECT
    il.id,
    il.work_item_id,
    il.config_snapshot_id,
    il.role,
    il.agent_type,
    n.provider,
    n.model_id,
    il.input_tokens,
    il.output_tokens,
    il.created_at,
    CASE WHEN p.input_per_1m IS NULL OR p.output_per_1m IS NULL THEN NULL
         ELSE (il.input_tokens::numeric * p.input_per_1m + il.output_tokens::numeric * p.output_per_1m) / 1000000
    END AS cost_usd
FROM inference_logs il
CROSS JOIN LATERAL (
    SELECT
        CASE WHEN position('/' IN il.model_provider) > 0 THEN split_part(il.model_provider, '/', 1) ELSE il.model_provider END AS provider,
        CASE WHEN il.model_id <> '' AND il.model_id <> il.model_provider THEN il.model_id
             WHEN position('/' IN il.model_provider) > 0 THEN substr(il.model_provider, position('/' IN il.model_provider) + 1)
             ELSE il.model_id END AS model_id
) n
LEFT JOIN LATERAL (
    SELECT mp.input_per_1m, mp.output_per_1m
    FROM model_prices mp
    WHERE mp.provider = n.provider AND mp.model_id = n.model_id AND mp.effective_from <= il.created_at
    ORDER BY mp.effective_from DESC
    LIMIT 1
) p ON true;

-- work_item_outcomes
CREATE TABLE IF NOT EXISTS work_item_outcomes (
    work_item_id             UUID        PRIMARY KEY,
    config_snapshot_id       TEXT,
    -- verified_head_sha: newest implementation_verifications head with
    -- outcome 'verified' (evidence only). reviewed_head_sha: the head the
    -- review examined (review artifact head_sha, D1.5). A reviewed item is
    -- not a verified one; rollups count "verified" as release_decision =
    -- 'release'.
    verified_head_sha        TEXT,
    reviewed_head_sha        TEXT,
    release_decision         TEXT,
    release_decisions        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    review_defects           JSONB,
    human_review_required    BOOLEAN     NOT NULL DEFAULT false,
    human_edits_after_review BOOLEAN,
    qa_result                TEXT,
    pr_number                INT,
    repository               TEXT,
    merged_head_sha          TEXT,
    merge_sha                TEXT,
    merged_at                TIMESTAMPTZ,
    reverted_at              TIMESTAMPTZ,
    revert_detection         TEXT,
    false_success            BOOLEAN     NOT NULL DEFAULT false,
    tokens_in                BIGINT      NOT NULL DEFAULT 0,
    tokens_out               BIGINT      NOT NULL DEFAULT 0,
    cost_usd                 NUMERIC(14,6),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT work_item_outcomes_work_item_id_fkey
        FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE,
    CONSTRAINT work_item_outcomes_config_snapshot_id_fkey
        FOREIGN KEY (config_snapshot_id) REFERENCES config_snapshots(id) ON DELETE SET NULL,
    CONSTRAINT work_item_outcomes_release_decision_check
        CHECK (release_decision IS NULL OR release_decision IN ('release', 'legacy_release', 'human_review', 'block')),
    CONSTRAINT work_item_outcomes_qa_result_check
        CHECK (qa_result IS NULL OR qa_result IN ('pass', 'fail')),
    CONSTRAINT work_item_outcomes_revert_detection_check
        CHECK (revert_detection IS NULL OR revert_detection IN ('heuristic'))
);

-- reviewed_head_sha was added to 053 after it was first applied to one
-- development database; the guarded ADD keeps a re-run converging.
ALTER TABLE work_item_outcomes ADD COLUMN IF NOT EXISTS reviewed_head_sha TEXT;

CREATE INDEX IF NOT EXISTS idx_work_item_outcomes_snapshot
    ON work_item_outcomes (config_snapshot_id);
CREATE INDEX IF NOT EXISTS idx_work_item_outcomes_pr
    ON work_item_outcomes (repository, pr_number) WHERE pr_number IS NOT NULL;
