CREATE TABLE onboarding_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    phase TEXT NOT NULL DEFAULT 'setup',
    prerequisites JSONB NOT NULL DEFAULT '{}',
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    started_by UUID REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_onboarding_sessions_product ON onboarding_sessions(product_id);
