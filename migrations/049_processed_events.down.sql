-- Rollback 049. Dropping the ledger forgets which events every consumer has
-- processed: until the services are rolled back too, a redelivered event
-- would be applied a second time.

DROP INDEX IF EXISTS idx_processed_events_processed_at;
DROP TABLE IF EXISTS processed_events;
