-- Rollback of 056: contract edges become ordinary plan edges (the rows stay,
-- so held consumers still wait for their producers) and the strict flag goes.

ALTER TABLE products DROP COLUMN IF EXISTS contracts_strict;

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_contract_ref_check;
ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_kind_check;
ALTER TABLE plan_task_dependencies DROP COLUMN IF EXISTS contract_check;
ALTER TABLE plan_task_dependencies DROP COLUMN IF EXISTS contract_ref;
ALTER TABLE plan_task_dependencies DROP COLUMN IF EXISTS kind;
