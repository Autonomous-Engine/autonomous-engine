-- Rollback 050. Dropping the tables loses every pinned base SHA and its audit
-- trail; in-flight items then behave as legacy-unpinned.

DROP INDEX IF EXISTS idx_base_sha_events_work_item_created;
DROP TABLE IF EXISTS base_sha_events;
DROP TABLE IF EXISTS work_item_base_shas;
