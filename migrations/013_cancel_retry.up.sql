-- Add 'cancelled' to work_item_state enum if not already present
ALTER TYPE work_item_state ADD VALUE IF NOT EXISTS 'cancelled';

-- Webhook dead-letter queue: stores events that failed to publish to Kafka
CREATE TABLE webhook_dead_letters (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source TEXT NOT NULL DEFAULT '',
    event_type TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL DEFAULT '{}',
    error_message TEXT NOT NULL DEFAULT '',
    retry_count INT NOT NULL DEFAULT 0,
    max_retries INT NOT NULL DEFAULT 3,
    next_retry_at TIMESTAMPTZ,
    resolved BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_webhook_dl_unresolved ON webhook_dead_letters(resolved, next_retry_at) WHERE NOT resolved;
