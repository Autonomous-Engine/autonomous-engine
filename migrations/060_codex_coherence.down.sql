-- 060 down: drop the codex pins and codex_versions.product_version.
-- work_items.codex_version keeps whatever product_version was written to it
-- (a display value; 0 for items planned before 060).
DROP INDEX IF EXISTS work_item_codex_pins_product_branch_idx;
DROP TABLE IF EXISTS work_item_codex_pins;

DROP INDEX IF EXISTS codex_versions_product_branch_version_key;
ALTER TABLE codex_versions DROP CONSTRAINT IF EXISTS codex_versions_product_version_check;
ALTER TABLE codex_versions DROP COLUMN IF EXISTS product_version;
