-- 063 down: back to migration 003/009's code_zones (global UNIQUE(name)).
--
-- Names that are now duplicated across repositories are suffixed with the
-- row id first, so the global unique constraint can be restored without
-- deleting any zone.

ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_repository_name_key;

UPDATE code_zones z
   SET name = z.name || ' (' || z.id::text || ')'
 WHERE EXISTS (SELECT 1 FROM code_zones o WHERE o.name = z.name AND o.id < z.id);

ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_name_key;
ALTER TABLE code_zones ADD CONSTRAINT code_zones_name_key UNIQUE (name);

ALTER TABLE code_zones DROP CONSTRAINT IF EXISTS code_zones_source_check;
ALTER TABLE code_zones DROP COLUMN IF EXISTS patterns;
ALTER TABLE code_zones DROP COLUMN IF EXISTS policy;
ALTER TABLE code_zones DROP COLUMN IF EXISTS description;
ALTER TABLE code_zones DROP COLUMN IF EXISTS source;
ALTER TABLE code_zones DROP COLUMN IF EXISTS updated_at;

DROP TYPE IF EXISTS zone_policy;

ALTER TABLE repositories DROP COLUMN IF EXISTS zones_confirmed_at;
