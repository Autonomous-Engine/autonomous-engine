-- 003_zones_external_changes.down.sql
-- Drop zones/external-change tables and types in reverse dependency order.

DROP TABLE IF EXISTS codex_submissions;
DROP TABLE IF EXISTS external_changes;
DROP TABLE IF EXISTS code_zones;

DROP TYPE IF EXISTS submission_status;
DROP TYPE IF EXISTS change_classification;
DROP TYPE IF EXISTS zone_status;
