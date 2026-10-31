-- 055 down: shadow evaluation and rollout gates (M4-06)

DROP INDEX IF EXISTS idx_work_items_replay_of;
DROP INDEX IF EXISTS idx_work_items_shadow_rollout;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_requested_config_snapshot_id_fkey;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_shadow_rollout_id_fkey;
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_replay_of_fkey;
ALTER TABLE work_items DROP COLUMN IF EXISTS requested_config_snapshot_id;
ALTER TABLE work_items DROP COLUMN IF EXISTS shadow_rollout_id;
ALTER TABLE work_items DROP COLUMN IF EXISTS replay_of;
ALTER TABLE work_items DROP COLUMN IF EXISTS replay;

ALTER TABLE products DROP COLUMN IF EXISTS benchmark_scratch;
ALTER TABLE products DROP COLUMN IF EXISTS allow_replay;

DROP TABLE IF EXISTS rollout_overrides;
DROP TABLE IF EXISTS rollouts;
