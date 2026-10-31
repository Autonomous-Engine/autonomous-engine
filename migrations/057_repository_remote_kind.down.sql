-- Rollback of 057: the remote kind label goes; runtime behaviour does not
-- depend on it (the kind is always derived from github_url).

ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_remote_kind_check;
ALTER TABLE repositories DROP COLUMN IF EXISTS remote_kind;
