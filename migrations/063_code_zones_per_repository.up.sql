-- 063: code_zones per repository, with patterns and a policy
-- Codex coherence, Part 2 (docs: codex coherence design).
--
-- Zones were schema only (migration 003: name globally UNIQUE, module_paths,
-- status; 009 added repository_id). They now decide where agents may change
-- code autonomously, where a human must approve the PR and where work stops:
--
--   patterns     repository-relative globs, verifypolicy.MatchPath semantics
--   policy       autonomous | human_review | escalate
--   description  operator text
--   source       config (codex-config.yaml defaults) | onboarding | dashboard
--   updated_at   last write
--
-- module_paths stays and is derived from patterns on every write
-- (internal/zones.DeriveModulePaths). status keeps active/frozen/thawing.
-- Names are unique per repository: the global UNIQUE(name) of 003
-- (default name code_zones_name_key) becomes UNIQUE(repository_id, name).
--
-- Existing rows (the engine never wrote any; an operator may have) get
-- patterns from their module_paths (each path and path/**) and the
-- human_review policy: the safest choice that does not stop work.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'zone_policy') THEN
        CREATE TYPE zone_policy AS ENUM ('autonomous', 'human_review', 'escalate');
    END IF;
END
$$;

ALTER TABLE code_zones ADD COLUMN IF NOT EXISTS patterns    TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE code_zones ADD COLUMN IF NOT EXISTS policy      zone_policy NOT NULL DEFAULT 'human_review';
ALTER TABLE code_zones ADD COLUMN IF NOT EXISTS description TEXT        NOT NULL DEFAULT '';
ALTER TABLE code_zones ADD COLUMN IF NOT EXISTS source      TEXT        NOT NULL DEFAULT 'config';
ALTER TABLE code_zones ADD COLUMN IF NOT EXISTS updated_at  TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_source_check;
ALTER TABLE code_zones ADD CONSTRAINT code_zones_source_check
    CHECK (source IN ('config', 'onboarding', 'dashboard'));

UPDATE code_zones
   SET patterns = ARRAY(
           SELECT DISTINCT p
             FROM unnest(module_paths) AS m(path),
                  LATERAL (VALUES (trim(both '/' from m.path)), (trim(both '/' from m.path) || '/**')) AS v(p)
            WHERE trim(both '/' from m.path) <> ''
            ORDER BY p
       )
 WHERE patterns = '{}'
   AND module_paths <> '{}';

ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_name_key;
ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_repository_name_key;
ALTER TABLE code_zones ADD CONSTRAINT code_zones_repository_name_key UNIQUE (repository_id, name);

-- repositories.zones_confirmed_at: when onboarding first set the
-- repository's zones (approval seeding or the wizard's confirmation, an
-- empty "no zones" choice included). Once set, onboarding may only add new
-- zones; seeding never re-applies the defaults; edits, weakening and
-- deletion go through the admin dashboard.
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS zones_confirmed_at TIMESTAMPTZ;
