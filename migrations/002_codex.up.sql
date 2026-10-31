-- 002_codex.up.sql
-- Codex knowledge-base: sections, versions, patterns, anti-patterns, modules.

CREATE TYPE codex_section_type AS ENUM (
    'architecture',
    'module',
    'pattern',
    'anti_pattern',
    'testing',
    'style',
    'process',
    'domain'
);

CREATE TYPE codex_change_source AS ENUM (
    'onboarding',
    'external_change',
    'human_update',
    'automated_reanalysis'
);

-- Top-level codex sections (living documents).
CREATE TABLE codex_sections (
    id           UUID               PRIMARY KEY DEFAULT gen_random_uuid(),
    section_type codex_section_type NOT NULL,
    name         TEXT               NOT NULL UNIQUE,
    content      JSONB              NOT NULL DEFAULT '{}',
    version      BIGINT             NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ        NOT NULL DEFAULT NOW()
);

-- Immutable version history of codex changes.
CREATE TABLE codex_versions (
    id               UUID               PRIMARY KEY DEFAULT gen_random_uuid(),
    version_number   SERIAL             UNIQUE NOT NULL,
    sections_changed TEXT[]             NOT NULL DEFAULT '{}',
    change_summary   TEXT               NOT NULL DEFAULT '',
    change_source    codex_change_source NOT NULL,
    diff             JSONB              NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ        NOT NULL DEFAULT NOW()
);

-- Reusable engineering patterns catalogued in the codex.
CREATE TABLE codex_patterns (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT        NOT NULL UNIQUE,
    category         TEXT        NOT NULL DEFAULT '',
    when_to_use      TEXT        NOT NULL DEFAULT '',
    reference_files  TEXT[]      NOT NULL DEFAULT '{}',
    rationale        TEXT        NOT NULL DEFAULT '',
    key_rules        TEXT[]      NOT NULL DEFAULT '{}',
    common_mistakes  TEXT[]      NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Anti-patterns to avoid, with historical context.
CREATE TABLE codex_anti_patterns (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT        NOT NULL UNIQUE,
    why_wrong          TEXT        NOT NULL DEFAULT '',
    what_instead       TEXT        NOT NULL DEFAULT '',
    historical_context TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Module registry: ownership, interfaces, and dependency graph.
CREATE TABLE codex_modules (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name                TEXT        NOT NULL UNIQUE,
    purpose             TEXT        NOT NULL DEFAULT '',
    public_interface    JSONB       NOT NULL DEFAULT '{}',
    dependencies        TEXT[]      NOT NULL DEFAULT '{}',
    dependents          TEXT[]      NOT NULL DEFAULT '{}',
    internal_patterns   TEXT        NOT NULL DEFAULT '',
    owner_domain        TEXT        NOT NULL DEFAULT '',
    risk_level          TEXT        NOT NULL DEFAULT 'medium',
    test_coverage_notes TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
