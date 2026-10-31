ALTER TABLE work_items DROP COLUMN IF EXISTS agent_definition_id;

DROP TABLE IF EXISTS agent_definition_prompts;

DROP INDEX IF EXISTS idx_agent_definitions_routing;
DROP INDEX IF EXISTS idx_agent_definitions_enabled;

DROP TABLE IF EXISTS agent_definitions;
