-- Products: groups of repos that form one logical product
CREATE TABLE products (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Repositories: individual git repos belonging to a product
CREATE TABLE repositories (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    github_url TEXT NOT NULL,
    base_branch TEXT NOT NULL DEFAULT 'main',
    merge_strategy TEXT NOT NULL DEFAULT 'squash',
    branch_prefix TEXT NOT NULL DEFAULT 'agent/',
    pr_template TEXT NOT NULL DEFAULT '',
    token_override TEXT NOT NULL DEFAULT '',
    clone_path TEXT NOT NULL DEFAULT '',
    last_indexed_at TIMESTAMPTZ,
    last_indexed_commit TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (product_id, github_url)
);

CREATE INDEX idx_repositories_product ON repositories(product_id);

-- Cross-repo dependency mapping
CREATE TABLE repo_dependencies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_repo_id UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    target_repo_id UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    dependency_type TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    discovered_by TEXT NOT NULL DEFAULT 'manual',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source_repo_id, target_repo_id)
);

-- Add product_id and repository_id to work_items
ALTER TABLE work_items ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE SET NULL;
ALTER TABLE work_items ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE SET NULL;
CREATE INDEX idx_work_items_product ON work_items(product_id);
CREATE INDEX idx_work_items_repository ON work_items(repository_id);

-- Add product_id and repository_id to Codex tables
ALTER TABLE codex_sections ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE CASCADE;
ALTER TABLE codex_sections ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE SET NULL;

ALTER TABLE codex_patterns ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE CASCADE;
ALTER TABLE codex_patterns ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE SET NULL;

ALTER TABLE codex_anti_patterns ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE CASCADE;
ALTER TABLE codex_anti_patterns ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE SET NULL;

ALTER TABLE codex_modules ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE CASCADE;
ALTER TABLE codex_modules ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE SET NULL;

ALTER TABLE codex_versions ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE CASCADE;

-- Add repository_id to code_zones
ALTER TABLE code_zones ADD COLUMN repository_id UUID REFERENCES repositories(id) ON DELETE CASCADE;

-- Add product_id to jira_ingested_tickets and portal_submissions
ALTER TABLE jira_ingested_tickets ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE SET NULL;
ALTER TABLE portal_submissions ADD COLUMN product_id UUID REFERENCES products(id) ON DELETE SET NULL;
