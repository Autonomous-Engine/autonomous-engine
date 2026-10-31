-- Per-product uniqueness for codex_modules, codex_patterns, and codex_sections.
--
-- Before this migration, codex_modules, codex_patterns, and codex_sections
-- each carried a plain UNIQUE constraint on (name). That was fine while the
-- system was single-product, but the moment a second product onboarded, a
-- module named "api" in product A would be silently overwritten when
-- product B's knowledge-indexer upserted its own "api" — along with every
-- field, including product_id and repository_id. Cross-product corruption,
-- latent behind any re-index.
--
-- codex_sections is included alongside modules/patterns here because
-- Cluster 3 adds product_id threading to UpsertSection at the same time,
-- and the collision risk is identical (e.g. two products each writing
-- a section named "architecture_overview" during onboarding).
--
-- This migration replaces the single-column unique constraint with two
-- partial unique indexes:
--
--   * codex_{modules,patterns}_product_name_key — UNIQUE (product_id, name)
--     where product_id IS NOT NULL. This is the intended multi-product key.
--
--   * codex_{modules,patterns}_name_global_key — UNIQUE (name) where
--     product_id IS NULL. This preserves the old invariant for legacy
--     rows that haven't yet been tagged with a product, so re-runs of
--     the non-scoped UpsertModule / UpsertPattern paths continue to
--     behave as they did before.
--
-- Once this migration lands, internal/codex/codex.go's ON CONFLICT clauses
-- must be updated in lockstep (ON CONFLICT (product_id, name) on the
-- -ForRepo variants, ON CONFLICT (name) WHERE product_id IS NULL on the
-- legacy variants). See Cluster 4 / Cluster 3 of the audit fix plan.

ALTER TABLE codex_modules  DROP CONSTRAINT IF EXISTS codex_modules_name_key;
ALTER TABLE codex_patterns DROP CONSTRAINT IF EXISTS codex_patterns_name_key;
ALTER TABLE codex_sections DROP CONSTRAINT IF EXISTS codex_sections_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS codex_modules_product_name_key
    ON codex_modules (product_id, name)
    WHERE product_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS codex_modules_name_global_key
    ON codex_modules (name)
    WHERE product_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS codex_patterns_product_name_key
    ON codex_patterns (product_id, name)
    WHERE product_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS codex_patterns_name_global_key
    ON codex_patterns (name)
    WHERE product_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS codex_sections_product_name_key
    ON codex_sections (product_id, name)
    WHERE product_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS codex_sections_name_global_key
    ON codex_sections (name)
    WHERE product_id IS NULL;
