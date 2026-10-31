-- Revert per-product uniqueness back to the global single-column constraint.
--
-- If duplicate names across products exist at down-migration time, the
-- ALTER ... ADD CONSTRAINT will fail. Resolve by merging or renaming the
-- duplicates before running this down migration.

DROP INDEX IF EXISTS codex_modules_product_name_key;
DROP INDEX IF EXISTS codex_modules_name_global_key;
DROP INDEX IF EXISTS codex_patterns_product_name_key;
DROP INDEX IF EXISTS codex_patterns_name_global_key;
DROP INDEX IF EXISTS codex_sections_product_name_key;
DROP INDEX IF EXISTS codex_sections_name_global_key;

ALTER TABLE codex_modules  ADD CONSTRAINT codex_modules_name_key  UNIQUE (name);
ALTER TABLE codex_patterns ADD CONSTRAINT codex_patterns_name_key UNIQUE (name);
ALTER TABLE codex_sections ADD CONSTRAINT codex_sections_name_key UNIQUE (name);
