-- 045: products.partial_verification_opt_in
-- Per-product opt-in for partially_verified implementation evidence
-- (an explicit per-product opt-in). Default false: a run that could not execute
-- every required check is blocked. When true, such a run may advance to
-- mandatory human review (never to automatic release); see
-- internal/evidence.ReleaseDecision.
--
-- Additive and re-runnable: no enum changes, no constraints, no backfill, so
-- the runner applies it inside a single transaction.

ALTER TABLE products
    ADD COLUMN IF NOT EXISTS partial_verification_opt_in BOOLEAN NOT NULL DEFAULT false;
