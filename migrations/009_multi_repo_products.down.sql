-- Reverse product_id from jira_ingested_tickets and portal_submissions
ALTER TABLE portal_submissions DROP COLUMN IF EXISTS product_id;
ALTER TABLE jira_ingested_tickets DROP COLUMN IF EXISTS product_id;

-- Reverse repository_id from code_zones
ALTER TABLE code_zones DROP COLUMN IF EXISTS repository_id;

-- Reverse product_id from codex_versions
ALTER TABLE codex_versions DROP COLUMN IF EXISTS product_id;

-- Reverse product_id and repository_id from codex_modules
ALTER TABLE codex_modules DROP COLUMN IF EXISTS repository_id;
ALTER TABLE codex_modules DROP COLUMN IF EXISTS product_id;

-- Reverse product_id and repository_id from codex_anti_patterns
ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS repository_id;
ALTER TABLE codex_anti_patterns DROP COLUMN IF EXISTS product_id;

-- Reverse product_id and repository_id from codex_patterns
ALTER TABLE codex_patterns DROP COLUMN IF EXISTS repository_id;
ALTER TABLE codex_patterns DROP COLUMN IF EXISTS product_id;

-- Reverse product_id and repository_id from codex_sections
ALTER TABLE codex_sections DROP COLUMN IF EXISTS repository_id;
ALTER TABLE codex_sections DROP COLUMN IF EXISTS product_id;

-- Reverse product_id and repository_id from work_items
DROP INDEX IF EXISTS idx_work_items_repository;
DROP INDEX IF EXISTS idx_work_items_product;
ALTER TABLE work_items DROP COLUMN IF EXISTS repository_id;
ALTER TABLE work_items DROP COLUMN IF EXISTS product_id;

-- Drop repo_dependencies, repositories, products
DROP TABLE IF EXISTS repo_dependencies;
DROP INDEX IF EXISTS idx_repositories_product;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS products;
