-- R14 down: drop the verification results. PostgreSQL cannot remove an enum
-- value, so 'verification' stays in work_item_state (as for awaiting_user,
-- migration 025); no row may be left in that state before rolling back.
DROP INDEX IF EXISTS verification_results_task_idx;
DROP INDEX IF EXISTS verification_results_head_idx;
DROP TABLE IF EXISTS verification_results;
