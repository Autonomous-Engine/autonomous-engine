-- 046: pending_pr_changes.head_sha
-- The exact verified head SHA (the SHA handed to review and PR services) of the branch a
-- gated PR would be opened from, copied from the implementation evidence by
-- pr-coordinator when the work item's pr_gate_mode is manual. Nullable:
-- rows written by pre-evidence producers, or under the legacy compatibility
-- window, have no verified SHA to record. Nothing else in pending_pr_changes
-- (files_changed, diff_stats, pr_title, pr_description) is a SHA slot.
--
-- Additive and re-runnable: no enum changes, no constraints, no backfill, so
-- the runner applies it inside a single transaction. pr-coordinator falls
-- back to the pre-046 column set if it is deployed before this runs.

ALTER TABLE pending_pr_changes
    ADD COLUMN IF NOT EXISTS head_sha TEXT;
