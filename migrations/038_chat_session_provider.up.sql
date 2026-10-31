ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS llm_provider_id UUID REFERENCES llm_providers(id) ON DELETE SET NULL;
