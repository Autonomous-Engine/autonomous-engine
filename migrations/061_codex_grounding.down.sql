-- 061 down: drop the codex grounding refusals and the codex gap records
-- (tables, their columns and every index of 061).
DROP INDEX IF EXISTS codex_gap_events_work_item_idx;
DROP TABLE IF EXISTS codex_gap_events;
DROP TABLE IF EXISTS codex_gap_runs;
DROP INDEX IF EXISTS codex_grounding_refusals_unposted_idx;
DROP INDEX IF EXISTS codex_grounding_refusals_reindex_idx;
DROP TABLE IF EXISTS codex_grounding_refusals;
