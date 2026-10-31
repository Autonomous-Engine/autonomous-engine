CREATE TABLE IF NOT EXISTS service_health_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('healthy', 'unhealthy', 'crashed', 'recovered')),
    message TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_service_health_events_service
    ON service_health_events (service_name, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_service_health_events_created
    ON service_health_events (created_at DESC);
