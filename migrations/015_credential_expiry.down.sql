DROP TABLE IF EXISTS credential_expiry_alerts_fired;
DROP INDEX IF EXISTS credential_registry_validation_idx;
ALTER TABLE credential_registry
  DROP COLUMN IF EXISTS last_validation_error,
  DROP COLUMN IF EXISTS last_validation_status,
  DROP COLUMN IF EXISTS last_validated_at,
  DROP COLUMN IF EXISTS expires_at;
