-- 047: outbox_events
-- The transactional outbox (roadmap 1.1, ticket M1-01). A producer writes the
-- state change and the event it causes in one transaction; the dispatcher
-- (M1-02) publishes the row to Kafka afterwards and marks it published only
-- once the bus acknowledges it. Delivery is therefore at-least-once, and `id`
-- is the stable event id consumers deduplicate on (M1-05): it is copied into
-- the Kafka headers and, for JSON-object payloads, into a top-level
-- "event_id" field.
--
-- No foreign key: an event must be able to outlive the row it describes (a
-- work item deleted before the dispatcher drains the queue must still deliver
-- its last events) and, for aggregates that are not work items, may have no
-- parent row in this database at all. A FK with ON DELETE CASCADE would
-- silently drop undelivered events; RESTRICT would block deletes on a queue
-- backlog. `aggregate_type` records what `aggregate_id` refers to instead.
--
-- No producer writes to this table until M1-03, so the down migration is a
-- clean rollback.

CREATE TABLE IF NOT EXISTS outbox_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    topic           TEXT        NOT NULL,
    key             TEXT        NOT NULL DEFAULT '',
    payload         JSONB       NOT NULL,
    headers         JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at      TIMESTAMPTZ,
    claimed_by      TEXT,
    published_at    TIMESTAMPTZ,
    dead_at         TIMESTAMPTZ,
    last_error      TEXT
);

-- The dispatcher's claim query scans only undelivered rows, oldest eligible
-- first, so the working index is partial: published and dead rows leave it as
-- soon as they are marked, and the index stays the size of the backlog rather
-- than the size of the table.
CREATE INDEX IF NOT EXISTS idx_outbox_events_ready
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL AND dead_at IS NULL;

-- Per-aggregate ordering: for each candidate row the claim query asks whether
-- an earlier unpublished row exists for the same aggregate.
CREATE INDEX IF NOT EXISTS idx_outbox_events_aggregate
    ON outbox_events (aggregate_id, created_at);
