-- 056: cross-repository contract edges (ticket M2-06, roadmap 2.3)
--
-- plan_task_dependencies (048) gains the edge's kind:
--   plan     - an ordering edge from the model's depends_on titles (M1-06);
--   contract - the consumer subtask depends on the producer subtask of a
--              contract between their repositories (codex_contract_edges or a
--              criterion's contracts). A pair that has both is one row of kind
--              contract (the primary key is the pair).
-- contract_ref is a JSON array of the contracts the edge carries
-- ({type, name, producer_repo_id, consumer_repo_id, source, ...};
-- internal/contracts.Ref); NULL for a plan edge. contract_check is the last
-- PR-time compatibility check of the edge (pr-coordinator): {status,
-- checked_at, results[]}; NULL until checked.
--
-- products.contracts_strict: when true, a contract the PR-time checkers
-- cannot evaluate ("unchecked") blocks the consumer's PR like a mismatch.
-- Default false: unchecked is recorded on the PR and in pr.ready only.
--
-- Additive and re-runnable; existing rows become kind 'plan'.

ALTER TABLE plan_task_dependencies
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'plan';

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_kind_check;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_kind_check
    CHECK (kind IN ('plan', 'contract'));

ALTER TABLE plan_task_dependencies
    ADD COLUMN IF NOT EXISTS contract_ref JSONB NULL;

ALTER TABLE plan_task_dependencies
    ADD COLUMN IF NOT EXISTS contract_check JSONB NULL;

-- A contract edge always names its contracts.
ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_contract_ref_check;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_contract_ref_check
    CHECK (kind <> 'contract' OR contract_ref IS NOT NULL);

ALTER TABLE products
    ADD COLUMN IF NOT EXISTS contracts_strict BOOLEAN NOT NULL DEFAULT false;
