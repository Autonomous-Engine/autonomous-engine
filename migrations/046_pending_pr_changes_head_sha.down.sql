-- Rollback of 046: drops the verified head SHA recorded on gated PR rows.
-- pr-coordinator detects the missing column and records pending changes
-- without it; the SHA remains in pr_description.

ALTER TABLE IF EXISTS pending_pr_changes
    DROP COLUMN IF EXISTS head_sha;
