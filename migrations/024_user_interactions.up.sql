-- user_interactions: every prompt + its lifecycle + its write-back outcome.
-- codex_entry_id + codex_entry_table together form a polymorphic back-link
-- to one of the per-section codex_* tables, because the codebase stores
-- typed codex entries in section-specific tables rather than a unified
-- codex_entries table.
CREATE TABLE user_interactions (
  id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  work_item_id               UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  product_id                 UUID NOT NULL REFERENCES products(id),
  asking_agent               TEXT NOT NULL,
  trigger                    TEXT NOT NULL,
  prompt_type                TEXT NOT NULL,

  headline                   TEXT NOT NULL,
  question                   TEXT NOT NULL,
  context_link               TEXT,
  context_payload            JSONB NOT NULL DEFAULT '{}'::jsonb,
  actions                    JSONB NOT NULL DEFAULT '[]'::jsonb,

  state                      TEXT NOT NULL,
  requested_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  answered_at                TIMESTAMPTZ,
  prompted_via               TEXT[],

  answered_by                TEXT,
  answered_via               TEXT,
  action_chosen              TEXT,
  answer_text                TEXT,
  save_as_override           TEXT,

  classified_as              TEXT,
  classification_confidence  NUMERIC(3,2),
  codex_entry_id             UUID,
  codex_entry_table          TEXT,         -- 'codex_modules' | 'codex_patterns' | 'codex_anti_patterns' | 'codex_sections'
  qdrant_point_id            TEXT,
  revoked_at                 TIMESTAMPTZ,
  revoked_by                 TEXT,
  revocation_reason          TEXT
);

CREATE INDEX user_interactions_state_pending_idx ON user_interactions(state) WHERE state = 'pending';
CREATE INDEX user_interactions_work_item_idx     ON user_interactions(work_item_id);
CREATE INDEX user_interactions_product_idx       ON user_interactions(product_id);
CREATE INDEX user_interactions_codex_entry_idx   ON user_interactions(codex_entry_id, codex_entry_table) WHERE codex_entry_id IS NOT NULL;

-- Add source-tracking columns to every codex section table. Four tables;
-- identical column set on each so downstream code can assume the fields
-- exist regardless of which section an entry lives in.
--   source                — 'auto_indexed' | 'user_interaction' | 'manual'
--   source_interaction_id — FK back to user_interactions when source='user_interaction'
--   state                 — 'active' | 'deprecated' | 'revoked'

ALTER TABLE codex_modules       ADD COLUMN source                TEXT NOT NULL DEFAULT 'auto_indexed';
ALTER TABLE codex_modules       ADD COLUMN source_interaction_id UUID REFERENCES user_interactions(id);
ALTER TABLE codex_modules       ADD COLUMN state                 TEXT NOT NULL DEFAULT 'active';

ALTER TABLE codex_patterns      ADD COLUMN source                TEXT NOT NULL DEFAULT 'auto_indexed';
ALTER TABLE codex_patterns      ADD COLUMN source_interaction_id UUID REFERENCES user_interactions(id);
ALTER TABLE codex_patterns      ADD COLUMN state                 TEXT NOT NULL DEFAULT 'active';

ALTER TABLE codex_anti_patterns ADD COLUMN source                TEXT NOT NULL DEFAULT 'auto_indexed';
ALTER TABLE codex_anti_patterns ADD COLUMN source_interaction_id UUID REFERENCES user_interactions(id);
ALTER TABLE codex_anti_patterns ADD COLUMN state                 TEXT NOT NULL DEFAULT 'active';

ALTER TABLE codex_sections      ADD COLUMN source                TEXT NOT NULL DEFAULT 'auto_indexed';
ALTER TABLE codex_sections      ADD COLUMN source_interaction_id UUID REFERENCES user_interactions(id);
ALTER TABLE codex_sections      ADD COLUMN state                 TEXT NOT NULL DEFAULT 'active';

-- work_items additions: track awaiting-user state
ALTER TABLE work_items ADD COLUMN previous_state      TEXT;
ALTER TABLE work_items ADD COLUMN awaiting_prompt_id  UUID REFERENCES user_interactions(id);

-- agent_resumption_state: persisted agent context across user waits
CREATE TABLE agent_resumption_state (
  prompt_id        UUID PRIMARY KEY REFERENCES user_interactions(id) ON DELETE CASCADE,
  work_item_id     UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  asking_agent     TEXT NOT NULL,
  message_history  JSONB NOT NULL,
  tool_call_stack  JSONB NOT NULL,
  saved_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX agent_resumption_state_work_item_idx ON agent_resumption_state(work_item_id);
