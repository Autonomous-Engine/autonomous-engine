-- 022_embedding_providers: down
--
-- Drops the embedding_providers table. Postgres does NOT support removing a
-- value from an enum type, so the 'embeddings' entry added to
-- credential_category remains present after a down migration. Re-applying
-- 022.up.sql is safe because the ALTER TYPE uses IF NOT EXISTS.

DROP TABLE IF EXISTS embedding_providers;
