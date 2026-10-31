-- Rollback of 053. Drops the view before the columns it reads, the outcome
-- and price tables, the linkage columns, then the snapshots.

DROP VIEW IF EXISTS inference_log_costs;

ALTER TABLE IF EXISTS work_item_outcomes DROP CONSTRAINT IF EXISTS work_item_outcomes_work_item_id_fkey;
ALTER TABLE IF EXISTS work_item_outcomes DROP CONSTRAINT IF EXISTS work_item_outcomes_config_snapshot_id_fkey;
DROP INDEX IF EXISTS idx_work_item_outcomes_snapshot;
DROP INDEX IF EXISTS idx_work_item_outcomes_pr;
DROP TABLE IF EXISTS work_item_outcomes;

DROP TABLE IF EXISTS model_prices;

DROP INDEX IF EXISTS idx_inference_logs_config_snapshot;
DROP INDEX IF EXISTS idx_inference_logs_work_item;
ALTER TABLE IF EXISTS inference_logs DROP CONSTRAINT IF EXISTS inference_logs_role_check;
ALTER TABLE IF EXISTS inference_logs DROP CONSTRAINT IF EXISTS inference_logs_config_snapshot_id_fkey;
ALTER TABLE IF EXISTS inference_logs DROP COLUMN IF EXISTS agent_type;
ALTER TABLE IF EXISTS inference_logs DROP COLUMN IF EXISTS role;
ALTER TABLE IF EXISTS inference_logs DROP COLUMN IF EXISTS config_snapshot_id;

ALTER TABLE IF EXISTS implementation_verifications DROP CONSTRAINT IF EXISTS implementation_verifications_config_snapshot_id_fkey;
ALTER TABLE IF EXISTS implementation_verifications DROP COLUMN IF EXISTS config_snapshot_id;

ALTER TABLE IF EXISTS implementation_runs DROP CONSTRAINT IF EXISTS implementation_runs_config_snapshot_id_fkey;
ALTER TABLE IF EXISTS implementation_runs DROP COLUMN IF EXISTS config_snapshot_id;

DROP INDEX IF EXISTS idx_work_items_config_snapshot;
ALTER TABLE IF EXISTS work_items DROP CONSTRAINT IF EXISTS work_items_config_snapshot_id_fkey;
ALTER TABLE IF EXISTS work_items DROP COLUMN IF EXISTS config_snapshot_id;

DROP INDEX IF EXISTS idx_config_snapshots_definition;
DROP INDEX IF EXISTS idx_config_snapshots_created_at;
DROP TABLE IF EXISTS config_snapshots;
