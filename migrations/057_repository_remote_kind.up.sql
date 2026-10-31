-- 057: repositories.remote_kind (ticket G1)
--
-- The kind of remote repositories.github_url names, as classified by
-- internal/gitremote.Classify when the repository is created or its URL is
-- changed:
--   github_shorthand - owner/repo (expands to https://github.com/owner/repo)
--   github_https     - http(s) URL whose host is exactly github.com
--   github_ssh       - git@github.com:o/r or ssh://git@github.com/o/r
--   local_path       - a filesystem path (local or bare mirror)
--   file_url         - a file:// URL
--   other_remote      - any network remote not on github.com
-- Only the github_* kinds ever get the GitHub PAT or the GitHub API (PR
-- create/merge); the others go through the manual PR gate. "invalid" is
-- rejected at creation and is never stored.
--
-- Nullable: rows created before 057 keep NULL. Every runtime decision derives
-- the kind from github_url (gitremote.Classify), so a NULL or stale label is
-- never trusted for a security decision; the column is the recorded label.
--
-- Additive and re-runnable.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS remote_kind TEXT NULL;

ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_remote_kind_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_remote_kind_check
    CHECK (remote_kind IS NULL OR remote_kind IN
        ('github_shorthand', 'github_https', 'github_ssh', 'local_path', 'file_url', 'other_remote'));
