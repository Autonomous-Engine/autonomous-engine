-- 005_dashboard_auth.down.sql
-- Drop dashboard/auth tables and types in reverse dependency order.

DROP TABLE IF EXISTS alert_history;
DROP TABLE IF EXISTS alert_configs;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

DROP TYPE IF EXISTS user_role;
