-- Dedicated table for credential-monitor alerts. Keeps the JSON payload
-- self-contained without needing to shoehorn into the operational alert_history
-- schema (which has a NOT NULL alert_config_id FK we'd have to seed against).

CREATE TABLE IF NOT EXISTS credential_alerts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credential_id  UUID REFERENCES credential_registry(id) ON DELETE CASCADE,
    category       TEXT NOT NULL,
    key_name       TEXT NOT NULL,
    severity       TEXT NOT NULL CHECK (severity IN ('info','warning','critical')),
    status         TEXT NOT NULL CHECK (status IN ('expiring_soon','expired','invalid','recovered')),
    message        TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    acknowledged   BOOLEAN NOT NULL DEFAULT false,
    fired_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS credential_alerts_fired_at_idx ON credential_alerts (fired_at DESC);
CREATE INDEX IF NOT EXISTS credential_alerts_active_idx
  ON credential_alerts (credential_id, status)
  WHERE acknowledged = false;
