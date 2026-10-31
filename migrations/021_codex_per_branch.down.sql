-- 021_codex_per_branch.down.sql
-- Reverses 021_codex_per_branch.up.sql. Drops the branch column and restores
-- the (product_id, name) unique indexes from migration 020 so a rollback
-- does not leave the database in an inconsistent state.

-- Drop branch-scoped lookup indexes first (harmless if absent).
DROP INDEX IF EXISTS codex_modules_branch_idx;
DROP INDEX IF EXISTS codex_patterns_branch_idx;
DROP INDEX IF EXISTS codex_sections_branch_idx;
DROP INDEX IF EXISTS codex_anti_patterns_branch_idx;
DROP INDEX IF EXISTS codex_versions_branch_idx;
DROP INDEX IF EXISTS work_items_branch_idx;

-- Drop the branch-aware unique indexes.
DROP INDEX IF EXISTS codex_modules_product_branch_name_key;
DROP INDEX IF EXISTS codex_patterns_product_branch_name_key;
DROP INDEX IF EXISTS codex_sections_product_branch_name_key;
DROP INDEX IF EXISTS codex_anti_patterns_product_branch_name_key;
DROP INDEX IF EXISTS codex_anti_patterns_name_global_key;

-- Restore the product-scoped unique indexes from migration 020.
CREATE UNIQUE INDEX IF NOT EXISTS codex_modules_product_name_key
    ON codex_modules(product_id, name)
    WHERE product_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS codex_patterns_product_name_key
    ON codex_patterns(product_id, name)
    WHERE product_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS codex_sections_product_name_key
    ON codex_sections(product_id, name)
    WHERE product_id IS NOT NULL;

-- Restore the single-column UNIQUE constraint on codex_anti_patterns.name
-- as it existed before migration 021. Uses a named constraint so a
-- re-up+down cycle does not pile up new constraints.
ALTER TABLE codex_anti_patterns ADD CONSTRAINT codex_anti_patterns_name_key UNIQUE (name);

-- Drop the branch columns. These were added with DEFAULT 'main' NOT NULL,
-- so simply dropping them removes all branch data.
ALTER TABLE work_items          DROP COLUMN IF EXISTS branch;
ALTER TABLE codex_versions      DROP COLUMN IF EXISTS branch;
ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS branch;
ALTER TABLE codex_sections      DROP COLUMN IF EXISTS branch;
ALTER TABLE codex_patterns      DROP COLUMN IF EXISTS branch;
ALTER TABLE codex_modules       DROP COLUMN IF EXISTS branch;
