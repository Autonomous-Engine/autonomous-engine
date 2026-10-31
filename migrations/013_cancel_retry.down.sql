-- PostgreSQL cannot remove enum values; 'cancelled' will remain but won't be used after rollback
DROP TABLE IF EXISTS webhook_dead_letters;
