-- 022_embedding_providers: first-class embedding provider configuration.
--
-- Mirrors the llm_providers pattern (migration 014) so the embedding service
-- used by knowledge-indexer / codex_similar_code is DB-configured instead of
-- env-var-configured. Every agent / indexer resolves its embedding config from
-- the single row where is_default = true AND enabled = true.

-- Add 'embeddings' to the credential_category enum. Using ADD VALUE IF NOT
-- EXISTS so the migration is idempotent (safe to re-run). The enum value
-- cannot be dropped by Postgres once added, so the .down.sql leaves it in
-- place — this is documented.
ALTER TYPE credential_category ADD VALUE IF NOT EXISTS 'embeddings';

CREATE TABLE embedding_providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    provider_type TEXT NOT NULL,           -- 'ollama' | 'openai' | 'azure_openai' | 'openai_compatible'
    endpoint TEXT NOT NULL,                -- full base URL; no trailing /embeddings
    model TEXT NOT NULL,                   -- e.g. 'nomic-embed-text:latest', 'text-embedding-3-small'
    dimension INTEGER NOT NULL DEFAULT 0,  -- detected vector length; 0 means "not yet tested"
    api_key_credential_id UUID REFERENCES credential_registry(id) ON DELETE SET NULL,
    is_default BOOLEAN NOT NULL DEFAULT false,
    enabled BOOLEAN NOT NULL DEFAULT true,
    settings JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one default at a time. Partial unique index ignores rows where
-- is_default = false so multiple non-defaults coexist freely.
CREATE UNIQUE INDEX idx_embedding_providers_one_default
    ON embedding_providers(is_default) WHERE is_default = true;

CREATE INDEX idx_embedding_providers_enabled
    ON embedding_providers(enabled) WHERE enabled = true;
