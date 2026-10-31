ALTER TABLE credential_registry
  ADD COLUMN expires_at              TIMESTAMPTZ,
  ADD COLUMN last_validated_at       TIMESTAMPTZ,
  ADD COLUMN last_validation_status  TEXT
    CHECK (last_validation_status IN ('unknown','valid','expired','invalid','network_error')),
  ADD COLUMN last_validation_error   TEXT;

CREATE INDEX credential_registry_validation_idx
  ON credential_registry (last_validated_at, category);

-- Per-credential per-threshold dedupe table for expiry warnings (14d, 7d, 1d, 0d).
CREATE TABLE credential_expiry_alerts_fired (
    credential_id UUID NOT NULL REFERENCES credential_registry(id) ON DELETE CASCADE,
    threshold_days INT NOT NULL CHECK (threshold_days IN (14, 7, 1, 0)),
    fired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at_snapshot TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (credential_id, threshold_days, expires_at_snapshot)
);
