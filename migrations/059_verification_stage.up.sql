-- R14: verification runs before the PR.
--
-- The independent verifier (M3-07) ran on pr.ready, after the PR (or the
-- manual PR gate) existed: with a local remote the gate could not be
-- approved, so the verifier never ran, and a change reached the gate as
-- "verified" because only its test commands had run. The verification stage
-- now consumes implementation.complete, verifies each subtask's verified
-- head before any PR exists, and announces verification.complete; the
-- pr-coordinator opens a PR (or records the pending change) only on an
-- approve decision.
--
-- 1. work_item_state gains 'verification': the parent's state between
--    implementation and review (or human_review / blocked). ADD VALUE runs
--    outside the transaction (internal/db/migrate.go).
-- 2. verification_results records one row per verification.complete: the
--    decision for a subtask's head SHA and the verdict artifact behind it.
--    status-orchestrator settles the parent from these rows (restart-safe),
--    pr-coordinator reads them when it missed the event, and review-agent
--    reuses the verdict for the PR's head instead of verifying again.

ALTER TYPE work_item_state ADD VALUE IF NOT EXISTS 'verification' AFTER 'implementation';

CREATE TABLE IF NOT EXISTS verification_results (
    id                  UUID PRIMARY KEY,
    work_item_id        UUID NOT NULL,
    task_id             UUID NOT NULL,
    head_sha            TEXT NOT NULL DEFAULT '',
    decision            TEXT NOT NULL,
    reason              TEXT NOT NULL DEFAULT '',
    legacy              BOOLEAN NOT NULL DEFAULT false,
    verdict_artifact_id UUID,
    verifier_version    TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT verification_results_work_item_id_fkey
        FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE,
    CONSTRAINT verification_results_task_id_fkey
        FOREIGN KEY (task_id) REFERENCES work_items(id) ON DELETE CASCADE,
    CONSTRAINT verification_results_verdict_artifact_id_fkey
        FOREIGN KEY (verdict_artifact_id) REFERENCES work_item_artifacts(id) ON DELETE SET NULL,
    CONSTRAINT verification_results_decision_check
        CHECK (decision IN ('approve', 'approve_pending_second_pass', 'request_changes', 'escalate', 'not_run', 'error'))
);

CREATE INDEX IF NOT EXISTS verification_results_head_idx
    ON verification_results (work_item_id, head_sha, created_at DESC);

CREATE INDEX IF NOT EXISTS verification_results_task_idx
    ON verification_results (task_id, created_at DESC);
