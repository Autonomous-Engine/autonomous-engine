-- Rollback of 045: drops the per-product partial-verification opt-in flag.
-- Consumers that cannot read the column fall back to the default policy
-- (opt-in false), which is the stricter behaviour.

ALTER TABLE IF EXISTS products
    DROP COLUMN IF EXISTS partial_verification_opt_in;
