-- 002_codex.down.sql
-- Drop codex tables and types in reverse dependency order.

DROP TABLE IF EXISTS codex_modules;
DROP TABLE IF EXISTS codex_anti_patterns;
DROP TABLE IF EXISTS codex_patterns;
DROP TABLE IF EXISTS codex_versions;
DROP TABLE IF EXISTS codex_sections;

DROP TYPE IF EXISTS codex_change_source;
DROP TYPE IF EXISTS codex_section_type;
