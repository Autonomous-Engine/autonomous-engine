-- 001_work_graph.up.sql
-- Work-item graph: types, core tables, transitions, artifacts, and assignments.

CREATE TYPE work_item_type AS ENUM (
    'feature',
    'bug',
    'task',
    'subtask',
    'research',
    'qa'
);

CREATE TYPE work_item_state AS ENUM (
    'intake',
    'prd',
    'research',
    'planning',
    'implementation',
    'review',
    'human_review',
    'qa',
    'passed',
    'failed',
    'blocked',
    'rejected',
    'released'
);

CREATE TYPE priority_level AS ENUM (
    'critical',
    'high',
    'medium',
    'low'
);

CREATE TYPE agent_type AS ENUM (
    'product',
    'research',
    'research_coordinator',
    'implementation',
    'implementation_planner',
    'pr_coordinator',
    'review',
    'qa'
);

CREATE TYPE assignment_status AS ENUM (
    'active',
    'completed',
    'failed',
    'cancelled'
);

CREATE TYPE artifact_type AS ENUM (
    'prd',
    'technical_brief',
    'research_finding',
    'qa_report',
    'review_comments'
);

-- Core work items table.
CREATE TABLE work_items (
    id           UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    external_ref TEXT          NOT NULL DEFAULT '',
    type         work_item_type NOT NULL,
    state        work_item_state NOT NULL DEFAULT 'intake',
    title        TEXT          NOT NULL DEFAULT '',
    description  TEXT          NOT NULL DEFAULT '',
    priority     priority_level NOT NULL DEFAULT 'medium',
    codex_version BIGINT       NOT NULL DEFAULT 0,
    parent_id    UUID          REFERENCES work_items(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

-- State transition history for work items.
CREATE TABLE work_item_transitions (
    id           UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID           NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    from_state   work_item_state,
    to_state     work_item_state NOT NULL,
    agent_id     TEXT           NOT NULL DEFAULT '',
    reason       TEXT           NOT NULL DEFAULT '',
    timestamp    TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

-- Artifacts produced by agents while working on an item.
CREATE TABLE work_item_artifacts (
    id           UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID          NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    artifact_type artifact_type NOT NULL,
    content      JSONB         NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    agent_id     TEXT          NOT NULL DEFAULT ''
);

-- Agent-to-work-item assignment records.
CREATE TABLE agent_assignments (
    id                UUID              PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id      UUID              NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    agent_type        agent_type        NOT NULL,
    agent_instance_id TEXT              NOT NULL DEFAULT '',
    assigned_at       TIMESTAMPTZ       NOT NULL DEFAULT NOW(),
    completed_at      TIMESTAMPTZ,
    status            assignment_status NOT NULL DEFAULT 'active'
);

-- Indexes for common query patterns.
CREATE INDEX idx_work_items_state        ON work_items(state);
CREATE INDEX idx_work_items_type         ON work_items(type);
CREATE INDEX idx_work_items_parent_id    ON work_items(parent_id);
CREATE INDEX idx_work_items_codex_version ON work_items(codex_version);
CREATE INDEX idx_work_items_priority     ON work_items(priority);
CREATE INDEX idx_work_items_created_at   ON work_items(created_at);

CREATE INDEX idx_transitions_work_item_id ON work_item_transitions(work_item_id);
CREATE INDEX idx_transitions_timestamp    ON work_item_transitions(timestamp);

CREATE INDEX idx_artifacts_work_item_id    ON work_item_artifacts(work_item_id);
CREATE INDEX idx_artifacts_artifact_type   ON work_item_artifacts(artifact_type);

CREATE INDEX idx_assignments_work_item_id     ON agent_assignments(work_item_id);
CREATE INDEX idx_assignments_status           ON agent_assignments(status);
CREATE INDEX idx_assignments_agent_instance_id ON agent_assignments(agent_instance_id);
