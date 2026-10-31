ALTER TABLE github_accounts ADD COLUMN IF NOT EXISTS label TEXT NOT NULL DEFAULT '';

ALTER TABLE github_accounts DROP CONSTRAINT IF EXISTS github_accounts_username_key;

CREATE UNIQUE INDEX IF NOT EXISTS github_accounts_username_label_key
    ON github_accounts (username, label);
