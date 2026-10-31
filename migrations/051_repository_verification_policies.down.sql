-- Rollback of 051: drops the per-repository verification policy table.
-- Consumers that cannot read it fall back to the fail-closed default
-- (internal/verifypolicy.Default: no executable checks -> block).

DROP TABLE IF EXISTS repository_verification_policies;
