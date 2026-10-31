DROP INDEX IF EXISTS agent_resumption_state_work_item_idx;
DROP TABLE IF EXISTS agent_resumption_state;

ALTER TABLE work_items DROP COLUMN IF EXISTS awaiting_prompt_id;
ALTER TABLE work_items DROP COLUMN IF EXISTS previous_state;

ALTER TABLE codex_sections      DROP COLUMN IF EXISTS state;
ALTER TABLE codex_sections      DROP COLUMN IF EXISTS source_interaction_id;
ALTER TABLE codex_sections      DROP COLUMN IF EXISTS source;

ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS state;
ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS source_interaction_id;
ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS source;

ALTER TABLE codex_patterns      DROP COLUMN IF EXISTS state;
ALTER TABLE codex_patterns      DROP COLUMN IF EXISTS source_interaction_id;
ALTER TABLE codex_patterns      DROP COLUMN IF EXISTS source;

ALTER TABLE codex_modules       DROP COLUMN IF EXISTS state;
ALTER TABLE codex_modules       DROP COLUMN IF EXISTS source_interaction_id;
ALTER TABLE codex_modules       DROP COLUMN IF EXISTS source;

DROP INDEX IF EXISTS user_interactions_codex_entry_idx;
DROP INDEX IF EXISTS user_interactions_product_idx;
DROP INDEX IF EXISTS user_interactions_work_item_idx;
DROP INDEX IF EXISTS user_interactions_state_pending_idx;
DROP TABLE IF EXISTS user_interactions;
