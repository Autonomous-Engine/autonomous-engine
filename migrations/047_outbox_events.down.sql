-- Rollback 047. Nothing produces or consumes outbox_events until M1-03, so
-- dropping the table loses no delivered state. After M1-03 a rollback must
-- first drain the queue (no rows with published_at IS NULL AND dead_at IS
-- NULL) or those events are lost.

DROP INDEX IF EXISTS idx_outbox_events_aggregate;
DROP INDEX IF EXISTS idx_outbox_events_ready;
DROP TABLE IF EXISTS outbox_events;
