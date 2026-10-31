-- 051: repository_verification_policies
-- Per-repository verification policy (M3-03; fail closed: no executable
-- checks blocks by default, security paths need review). One row per repository (1:1, primary key = repository_id). A
-- repository without a row is governed by the fail-closed default in
-- internal/verifypolicy.Default: no_checks_policy='block', required_checks
-- '{test}', the generic security-review globs.
--
--   test_command          operator override of test-runner detection; NULL
--                         means "use detection" (internal/testdetect).
--   required_checks       subset of evidence.CheckKind the operator requires
--                         ('test', 'build', 'lint'). Kinds the agent cannot run
--                         yet are recorded as not_run and cap the outcome at
--                         partially_verified.
--   no_checks_policy      what happens when no executable check exists:
--                         'block' (default, D2) or 'partial_with_human_review'
--                         (D2 opt-in; still requires the product's
--                         partial_verification_opt_in downstream).
--   security_review_paths glob list; a changed file matching any of them
--                         adds the security_review_required evidence flag and
--                         routes verified work to human review (D3).
--   set_by / set_at / source  audit trail: who set the policy, when, and
--                         through which surface.
--
-- Re-runnable: IF NOT EXISTS guards, constraints dropped before being added.

CREATE TABLE IF NOT EXISTS repository_verification_policies (
    repository_id         UUID        NOT NULL,
    test_command          TEXT        NULL,
    required_checks       TEXT[]      NOT NULL DEFAULT '{test}',
    no_checks_policy      TEXT        NOT NULL DEFAULT 'block',
    security_review_paths TEXT[]      NOT NULL DEFAULT '{}',
    set_by                TEXT,
    set_at                TIMESTAMPTZ,
    source                TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE repository_verification_policies
    DROP CONSTRAINT IF EXISTS repository_verification_policies_pkey;
ALTER TABLE repository_verification_policies
    ADD CONSTRAINT repository_verification_policies_pkey PRIMARY KEY (repository_id);

ALTER TABLE repository_verification_policies
    DROP CONSTRAINT IF EXISTS repository_verification_policies_repository_id_fkey;
ALTER TABLE repository_verification_policies
    ADD CONSTRAINT repository_verification_policies_repository_id_fkey
        FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE;

ALTER TABLE repository_verification_policies
    DROP CONSTRAINT IF EXISTS repository_verification_policies_no_checks_policy_check;
ALTER TABLE repository_verification_policies
    ADD CONSTRAINT repository_verification_policies_no_checks_policy_check
        CHECK (no_checks_policy IN ('block', 'partial_with_human_review'));

ALTER TABLE repository_verification_policies
    DROP CONSTRAINT IF EXISTS repository_verification_policies_source_check;
ALTER TABLE repository_verification_policies
    ADD CONSTRAINT repository_verification_policies_source_check
        CHECK (source IN ('onboarding', 'admin', 'default'));

ALTER TABLE repository_verification_policies
    DROP CONSTRAINT IF EXISTS repository_verification_policies_required_checks_check;
ALTER TABLE repository_verification_policies
    ADD CONSTRAINT repository_verification_policies_required_checks_check
        CHECK (required_checks <@ ARRAY['test', 'build', 'lint']::TEXT[]);
