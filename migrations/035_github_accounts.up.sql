-- Add 'github_account' to credential_scope enum
ALTER TYPE credential_scope ADD VALUE IF NOT EXISTS 'github_account';

-- GitHub accounts table
CREATE TABLE github_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL,
    avatar_url TEXT NOT NULL DEFAULT '',
    credential_id UUID NOT NULL REFERENCES credential_registry(id) ON DELETE CASCADE,
    last_validated_at TIMESTAMPTZ,
    valid BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (username)
);

-- Add github_account_id FK to repositories
ALTER TABLE repositories ADD COLUMN github_account_id UUID REFERENCES github_accounts(id) ON DELETE SET NULL;
