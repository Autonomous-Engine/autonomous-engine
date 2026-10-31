-- 021_codex_per_branch.up.sql
--
-- Branch-aware codex. Before this migration every codex row was scoped only
-- to (product_id, name); a feature branch had no way to register a module or
-- pattern that differed from main without overwriting main's row. Every agent
-- pulling codex context received the same grounding regardless of which
-- branch it was working on. That's wrong: a feature branch adds/removes
-- modules, changes patterns, and diverges from main during its lifetime.
--
-- This migration adds a `branch TEXT NOT NULL DEFAULT 'main'` column to each
-- codex table and replaces the product-scoped unique indexes from migration
-- 020 with (product_id, branch, name) triples. Existing rows default to
-- 'main' so every pre-021 caller continues to see exactly what it saw
-- before: main's codex.
--
-- The plan from the spec includes codex_versions as well so
-- CheckOnboardingComplete and the version-history machinery know which
-- branch is being versioned. The UpsertPattern/UpsertModule/UpsertSection
-- legacy (product_id IS NULL) paths keep their existing *_name_global_key
-- indexes unchanged — adding a branch column there would expand the legacy
-- namespace needlessly. Global rows collide across branches by name the same
-- way they always did.
--
-- work_items also learns a branch column so each agent's codex lookup can
-- scope to the branch the work item is targeting. NULL means "use main"
-- (the historical behaviour), a concrete value means "use this branch's
-- codex, fall back to main if no grounding exists yet."

ALTER TABLE codex_modules       ADD COLUMN IF NOT EXISTS branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE codex_patterns      ADD COLUMN IF NOT EXISTS branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE codex_sections      ADD COLUMN IF NOT EXISTS branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE codex_anti_patterns ADD COLUMN IF NOT EXISTS branch TEXT NOT NULL DEFAULT 'main';
ALTER TABLE codex_versions      ADD COLUMN IF NOT EXISTS branch TEXT NOT NULL DEFAULT 'main';

-- Replace product-scoped unique indexes from migration 020 with
-- (product_id, branch, name) unique indexes. IF EXISTS keeps this idempotent
-- on re-apply.
DROP INDEX IF EXISTS codex_modules_product_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS codex_modules_product_branch_name_key
    ON codex_modules(product_id, branch, name)
    WHERE product_id IS NOT NULL;

DROP INDEX IF EXISTS codex_patterns_product_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS codex_patterns_product_branch_name_key
    ON codex_patterns(product_id, branch, name)
    WHERE product_id IS NOT NULL;

DROP INDEX IF EXISTS codex_sections_product_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS codex_sections_product_branch_name_key
    ON codex_sections(product_id, branch, name)
    WHERE product_id IS NOT NULL;

-- codex_anti_patterns still carries the single-column codex_anti_patterns_name_key
-- unique constraint from migration 002 (product_id on the table is informational
-- only — no prior partial index was ever created). Replace it with a
-- (product_id, branch, name) partial index so anti-patterns are also
-- branch-aware. The global fallback (product_id IS NULL) is covered by a
-- separate name-only index for parity with patterns/modules/sections.
ALTER TABLE codex_anti_patterns DROP CONSTRAINT IF EXISTS codex_anti_patterns_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS codex_anti_patterns_product_branch_name_key
    ON codex_anti_patterns(product_id, branch, name)
    WHERE product_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS codex_anti_patterns_name_global_key
    ON codex_anti_patterns(name)
    WHERE product_id IS NULL;

-- Indexes for branch-scoped lookups so GetModulesForProductBranch /
-- GetPatternsForProductBranch etc don't scan the entire product partition.
CREATE INDEX IF NOT EXISTS codex_modules_branch_idx       ON codex_modules(product_id, branch);
CREATE INDEX IF NOT EXISTS codex_patterns_branch_idx      ON codex_patterns(product_id, branch);
CREATE INDEX IF NOT EXISTS codex_sections_branch_idx      ON codex_sections(product_id, branch);
CREATE INDEX IF NOT EXISTS codex_anti_patterns_branch_idx ON codex_anti_patterns(product_id, branch);
CREATE INDEX IF NOT EXISTS codex_versions_branch_idx      ON codex_versions(product_id, branch);

-- work_items learns a branch column so each agent's codex lookup can scope
-- to the branch the work item is targeting. NULL means "use main" (the
-- historical default); a concrete value means "use this branch's codex,
-- fall back to main if no grounding exists yet." A feature branch is
-- created by the implementation agent later in the pipeline, so we don't
-- require a value at work-item creation time.
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS branch TEXT;
CREATE INDEX IF NOT EXISTS work_items_branch_idx ON work_items(branch) WHERE branch IS NOT NULL;
