DROP INDEX IF EXISTS github_accounts_username_label_key;

ALTER TABLE github_accounts ADD CONSTRAINT github_accounts_username_key UNIQUE (username);

ALTER TABLE github_accounts DROP COLUMN IF EXISTS label;
