-- 060: codex_versions.product_version, work_item_codex_pins
-- Codex coherence, Part 0 (docs: codex coherence design).
--
-- 1. codex_versions.version_number is one global SERIAL, so "the current
--    codex" of a product and branch could not be told apart from another
--    product's writes. product_version numbers the versions of each
--    (product_id, branch) from 1; codex.RecordVersionTx assigns it under an
--    advisory lock keyed by product and branch, in the transaction that
--    inserts the row. version_number stays the row's global id.
--    Existing rows are numbered here in version_number order. Rows without
--    a product (legacy single-repo writes) keep NULL: no per-product reader
--    sees them, and the CHECK below makes every product row carry a number.
--    The unique index is the backstop for the lock.
-- 2. work_item_codex_pins records the codex a work item was planned
--    against: the product, branch and product_version of the technical
--    brief, and the modules the brief relies on. research-coordinator writes
--    the parent's pin with the brief; the planner copies it to each subtask.

ALTER TABLE codex_versions ADD COLUMN IF NOT EXISTS product_version INT;

UPDATE codex_versions cv
   SET product_version = n.rn
  FROM (
        SELECT id,
               ROW_NUMBER() OVER (PARTITION BY product_id, branch ORDER BY version_number) AS rn
          FROM codex_versions
         WHERE product_id IS NOT NULL
       ) n
 WHERE cv.id = n.id
   AND cv.product_version IS NULL;

ALTER TABLE codex_versions DROP CONSTRAINT IF EXISTS codex_versions_product_version_check;
ALTER TABLE codex_versions ADD CONSTRAINT codex_versions_product_version_check
    CHECK (product_id IS NULL OR (product_version IS NOT NULL AND product_version > 0));

CREATE UNIQUE INDEX IF NOT EXISTS codex_versions_product_branch_version_key
    ON codex_versions (product_id, branch, product_version)
    WHERE product_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS work_item_codex_pins (
    work_item_id    UUID        NOT NULL,
    product_id      UUID        NOT NULL,
    branch          TEXT        NOT NULL,
    product_version INT         NOT NULL,
    modules         TEXT[]      NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE work_item_codex_pins DROP CONSTRAINT IF EXISTS work_item_codex_pins_pkey;
ALTER TABLE work_item_codex_pins ADD CONSTRAINT work_item_codex_pins_pkey
    PRIMARY KEY (work_item_id);

ALTER TABLE work_item_codex_pins DROP CONSTRAINT IF EXISTS work_item_codex_pins_work_item_id_fkey;
ALTER TABLE work_item_codex_pins ADD CONSTRAINT work_item_codex_pins_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE work_item_codex_pins DROP CONSTRAINT IF EXISTS work_item_codex_pins_product_id_fkey;
ALTER TABLE work_item_codex_pins ADD CONSTRAINT work_item_codex_pins_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE;

-- 0 is allowed: the brief was produced before the branch had any version.
ALTER TABLE work_item_codex_pins DROP CONSTRAINT IF EXISTS work_item_codex_pins_product_version_check;
ALTER TABLE work_item_codex_pins ADD CONSTRAINT work_item_codex_pins_product_version_check
    CHECK (product_version >= 0);

ALTER TABLE work_item_codex_pins DROP CONSTRAINT IF EXISTS work_item_codex_pins_branch_check;
ALTER TABLE work_item_codex_pins ADD CONSTRAINT work_item_codex_pins_branch_check
    CHECK (branch <> '');

CREATE INDEX IF NOT EXISTS work_item_codex_pins_product_branch_idx
    ON work_item_codex_pins (product_id, branch);
