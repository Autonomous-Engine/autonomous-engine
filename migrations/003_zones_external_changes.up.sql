-- 003_zones_external_changes.up.sql
-- Code zones, external-change tracking, and codex submission workflow.

CREATE TYPE zone_status AS ENUM (
    'active',
    'frozen',
    'thawing'
);

CREATE TYPE change_classification AS ENUM (
    'minor',
    'moderate',
    'major'
);

CREATE TYPE submission_status AS ENUM (
    'pending',
    'submitted',
    'auto_generated',
    'reviewed'
);

-- Named code zones that group related module paths.
CREATE TABLE code_zones (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT        NOT NULL UNIQUE,
    module_paths  TEXT[]      NOT NULL DEFAULT '{}',
    status        zone_status NOT NULL DEFAULT 'active',
    frozen_reason TEXT,
    frozen_at     TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- External (e.g. PR-based) changes detected against tracked zones.
CREATE TABLE external_changes (
    id                UUID                 PRIMARY KEY DEFAULT gen_random_uuid(),
    pr_url            TEXT                 NOT NULL DEFAULT '',
    classification    change_classification NOT NULL,
    affected_zones    TEXT[]               NOT NULL DEFAULT '{}',
    submission_status submission_status    NOT NULL DEFAULT 'pending',
    detected_at       TIMESTAMPTZ          NOT NULL DEFAULT NOW(),
    submitted_at      TIMESTAMPTZ
);

-- Codex submissions generated from external changes.
CREATE TABLE codex_submissions (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    external_change_id UUID        NOT NULL REFERENCES external_changes(id) ON DELETE CASCADE,
    answers            JSONB       NOT NULL DEFAULT '{}',
    submitted_by       TEXT        NOT NULL DEFAULT '',
    submitted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_external_changes_submission_status ON external_changes(submission_status);
