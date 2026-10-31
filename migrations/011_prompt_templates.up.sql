CREATE TABLE prompt_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_type TEXT NOT NULL,
    agent_type TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    operating_mode TEXT NOT NULL DEFAULT '',
    prompt_type TEXT NOT NULL DEFAULT 'system',
    content TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (intent_type, agent_type, provider, model_id, operating_mode, prompt_type)
);

CREATE INDEX idx_prompt_templates_intent ON prompt_templates(intent_type);
CREATE INDEX idx_prompt_templates_lookup ON prompt_templates(intent_type, agent_type, provider, model_id, operating_mode);
