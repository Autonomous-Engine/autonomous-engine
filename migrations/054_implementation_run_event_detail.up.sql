-- 054: structured detail on implementation run events (M3-06)
-- The implementation agent's repair loop records each pre-commit check run
-- as a 'tool' event whose detail holds the normalised failure records
-- (internal/failures: fingerprint, category, file/line, ownership,
-- retryability, raw output ref), and each plan revision as a 'note' event
-- whose detail links it to the contradicting record (fingerprint + call
-- id). NULL for every other event and for rows written before this
-- migration. Per-run aggregates (fingerprint counts, repair attempts, the
-- D4 budget) live in implementation_runs.detail under "repair" and
-- "budget".

ALTER TABLE implementation_run_events ADD COLUMN IF NOT EXISTS detail JSONB NULL;
