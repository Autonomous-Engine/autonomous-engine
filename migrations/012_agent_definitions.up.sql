CREATE TABLE agent_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',

    -- Model configuration
    provider TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    model_endpoint TEXT NOT NULL DEFAULT '',

    -- Pipeline configuration: which steps to include
    -- Each is a boolean. If false, that step is skipped.
    step_product_analysis BOOLEAN NOT NULL DEFAULT true,
    step_research BOOLEAN NOT NULL DEFAULT true,
    step_planning BOOLEAN NOT NULL DEFAULT true,
    step_implementation BOOLEAN NOT NULL DEFAULT true,
    step_review BOOLEAN NOT NULL DEFAULT true,
    step_human_review BOOLEAN NOT NULL DEFAULT true,
    step_qa BOOLEAN NOT NULL DEFAULT true,

    -- Routing rules
    routing_tags TEXT[] NOT NULL DEFAULT '{}',
    routing_complexity TEXT[] NOT NULL DEFAULT '{}',
    routing_priority BOOLEAN NOT NULL DEFAULT false,

    -- Behavior
    max_retries INT NOT NULL DEFAULT 3,
    max_files_per_task INT NOT NULL DEFAULT 10,
    timeout_minutes INT NOT NULL DEFAULT 120,

    -- Status
    enabled BOOLEAN NOT NULL DEFAULT true,
    is_default BOOLEAN NOT NULL DEFAULT false,

    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_definitions_enabled ON agent_definitions(enabled);
CREATE INDEX idx_agent_definitions_routing ON agent_definitions USING gin(routing_tags);

-- Agent-specific prompts linked to definitions
CREATE TABLE agent_definition_prompts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_definition_id UUID NOT NULL REFERENCES agent_definitions(id) ON DELETE CASCADE,
    intent_type TEXT NOT NULL,
    prompt_type TEXT NOT NULL DEFAULT 'system',
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (agent_definition_id, intent_type, prompt_type)
);

-- Work items track which agent definition processed them
ALTER TABLE work_items ADD COLUMN agent_definition_id UUID REFERENCES agent_definitions(id) ON DELETE SET NULL;
