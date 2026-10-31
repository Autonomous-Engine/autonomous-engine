-- Restore the DEFAULT 'main' clause. Note the rollback does not rewrite
-- any existing rows — they keep whatever branch they were written with.
ALTER TABLE repositories ALTER COLUMN base_branch SET DEFAULT 'main';
