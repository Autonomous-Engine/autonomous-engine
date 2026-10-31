-- Proactive onboarding — pre-fill candidate answers from the knowledge-indexer
-- scan, one row per (product_id, question_key). Each row carries the system's
-- best answer (value), a coarse confidence bucket, the evidence array in JSONB,
-- and a user_status field the UI drives via /api/onboarding/candidates/review.
CREATE TABLE IF NOT EXISTS onboarding_candidates (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id       UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    repository_id    UUID REFERENCES repositories(id) ON DELETE CASCADE,
    question_key     TEXT NOT NULL,
    value            TEXT NOT NULL DEFAULT '',
    confidence       TEXT NOT NULL
                     CHECK (confidence IN ('high','medium','low','not_detected')),
    evidence         JSONB NOT NULL DEFAULT '[]',
    extractor        TEXT NOT NULL,
    user_status      TEXT NOT NULL DEFAULT 'pending'
                     CHECK (user_status IN ('pending','confirmed','edited','rejected')),
    user_value       TEXT,
    generated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at      TIMESTAMPTZ,
    UNIQUE (product_id, question_key)
);
CREATE INDEX IF NOT EXISTS idx_onboarding_candidates_product ON onboarding_candidates(product_id);
CREATE INDEX IF NOT EXISTS idx_onboarding_candidates_status  ON onboarding_candidates(product_id, user_status);
