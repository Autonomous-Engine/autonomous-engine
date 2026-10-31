ALTER TABLE repositories DROP COLUMN IF EXISTS github_account_id;
DROP TABLE IF EXISTS github_accounts;
-- Note: cannot remove enum value in PostgreSQL; leaving 'github_account' in credential_scope.
