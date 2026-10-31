-- 044: implementation_verifications
-- Persistent record of what an implementation run actually verified, linking
-- a work item to the exact head SHA and the checks that ran. The row mirrors
-- internal/evidence.ImplementationEvidence (schema_version 1); the scalar
-- columns are denormalised copies of fields inside `evidence` so they can be
-- indexed and filtered without unpacking the JSONB.
--
-- No consumer writes or reads this table yet (M0-07 migrates producers and
-- consumers); it is safe to roll back with the down migration.

CREATE TABLE IF NOT EXISTS implementation_verifications (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id   UUID        NOT NULL,
    task_id        TEXT        NOT NULL DEFAULT '',
    repository_id  UUID,
    schema_version INT         NOT NULL,
    base_sha       TEXT        NOT NULL DEFAULT '',
    head_sha       TEXT        NOT NULL DEFAULT '',
    branch         TEXT        NOT NULL DEFAULT '',
    outcome        TEXT        NOT NULL,
    evidence       JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT implementation_verifications_outcome_check
        CHECK (outcome IN ('verified', 'partially_verified', 'failed', 'unverified')),
    CONSTRAINT implementation_verifications_work_item_id_fkey
        FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE,
    CONSTRAINT implementation_verifications_repository_id_fkey
        FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_implementation_verifications_work_item
    ON implementation_verifications (work_item_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_implementation_verifications_head_sha
    ON implementation_verifications (head_sha);
