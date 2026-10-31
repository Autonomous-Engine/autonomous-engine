-- Rollback of 052: drops the run tables (events first) and the two
-- tool_events columns. Nothing else references these tables.

ALTER TABLE IF EXISTS tool_events DROP COLUMN IF EXISTS exit_code;
ALTER TABLE IF EXISTS tool_events DROP COLUMN IF EXISTS phase;

ALTER TABLE IF EXISTS implementation_run_events
    DROP CONSTRAINT IF EXISTS implementation_run_events_run_id_fkey;
ALTER TABLE IF EXISTS implementation_run_events
    DROP CONSTRAINT IF EXISTS implementation_run_events_kind_check;
DROP TABLE IF EXISTS implementation_run_events;

ALTER TABLE IF EXISTS implementation_runs
    DROP CONSTRAINT IF EXISTS implementation_runs_work_item_id_fkey;
ALTER TABLE IF EXISTS implementation_runs
    DROP CONSTRAINT IF EXISTS implementation_runs_task_attempt_key;
ALTER TABLE IF EXISTS implementation_runs
    DROP CONSTRAINT IF EXISTS implementation_runs_state_check;
DROP INDEX IF EXISTS idx_implementation_runs_work_item;
DROP TABLE IF EXISTS implementation_runs;
