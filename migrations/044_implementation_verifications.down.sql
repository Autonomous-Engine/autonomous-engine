-- Rollback of 044: drops the evidence table, its named constraints, and its
-- indexes. Nothing else references implementation_verifications, so no
-- constraints need restoring elsewhere.

ALTER TABLE IF EXISTS implementation_verifications
    DROP CONSTRAINT IF EXISTS implementation_verifications_outcome_check;
ALTER TABLE IF EXISTS implementation_verifications
    DROP CONSTRAINT IF EXISTS implementation_verifications_repository_id_fkey;
ALTER TABLE IF EXISTS implementation_verifications
    DROP CONSTRAINT IF EXISTS implementation_verifications_work_item_id_fkey;
DROP INDEX IF EXISTS idx_implementation_verifications_head_sha;
DROP INDEX IF EXISTS idx_implementation_verifications_work_item;
DROP TABLE IF EXISTS implementation_verifications;
