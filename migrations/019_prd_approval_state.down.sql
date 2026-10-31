-- Postgres doesn't natively support removing enum values. If you need to
-- roll back, restore from backup or manually migrate rows out of
-- 'prd_approval' first, drop + recreate the type, and recreate the column.
-- Providing an explicit no-op down so the migrate tool doesn't complain.
SELECT 1;
