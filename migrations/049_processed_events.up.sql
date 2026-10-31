-- 049: processed_events
-- The idempotency ledger for Kafka consumers (roadmap 1.2, ticket M1-05).
-- Delivery is at-least-once: Kafka redelivers anything whose offset was not
-- committed, and the transactional outbox (047) can publish an event twice
-- when a dispatcher crashes between the broker's ack and marking the row
-- published. Every outbox event carries a stable event_id; a consumer inserts
-- (consumer, event_id) here as the FIRST statement of the transaction that
-- applies the event's state change (internal/idempotency Guard), so
-- "processed" and the effects commit together, and a redelivered event finds
-- the row and is acknowledged without being applied again.
--
-- consumer is the service name (status-orchestrator, pr-coordinator, ...):
-- one event is processed once PER consumer, and the same id reaching two
-- services is two rows.
--
-- No foreign key: event ids are not rows of any table this database keeps
-- (outbox rows may be pruned long before the ledger is).
--
-- Retention: rows older than 30 days may be pruned by an operator job; no
-- automatic pruning exists. Kafka's own retention bounds how late a
-- duplicate can arrive, so a row only has to outlive that window:
--   DELETE FROM processed_events WHERE processed_at < now() - interval '30 days';
-- idx_processed_events_processed_at serves that delete.

CREATE TABLE IF NOT EXISTS processed_events (
    consumer     TEXT        NOT NULL,
    event_id     UUID        NOT NULL,
    topic        TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_pkey;
ALTER TABLE processed_events ADD CONSTRAINT processed_events_pkey
    PRIMARY KEY (consumer, event_id);

CREATE INDEX IF NOT EXISTS idx_processed_events_processed_at
    ON processed_events (processed_at);
