-- 004_model_metrics.down.sql
-- Drop model/metrics tables and types in reverse dependency order.

DROP TABLE IF EXISTS pipeline_metrics;
DROP TABLE IF EXISTS inference_logs;
DROP TABLE IF EXISTS model_profiles;

DROP TYPE IF EXISTS operating_mode;
