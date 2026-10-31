-- 001_work_graph.down.sql
-- Drop work-graph tables and types in reverse dependency order.

DROP TABLE IF EXISTS agent_assignments;
DROP TABLE IF EXISTS work_item_artifacts;
DROP TABLE IF EXISTS work_item_transitions;
DROP TABLE IF EXISTS work_items;

DROP TYPE IF EXISTS artifact_type;
DROP TYPE IF EXISTS assignment_status;
DROP TYPE IF EXISTS agent_type;
DROP TYPE IF EXISTS priority_level;
DROP TYPE IF EXISTS work_item_state;
DROP TYPE IF EXISTS work_item_type;
