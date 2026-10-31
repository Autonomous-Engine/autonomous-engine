-- PR generation gate: allows users to stop auto-PR creation and review changes manually.
-- work_items.pr_gate_mode: 'auto' (default, current behavior) or 'manual' (skip PR, let user review)
-- products.default_pr_gate_mode: default gate mode for new work items in this product

ALTER TABLE work_items ADD COLUMN IF NOT EXISTS pr_gate_mode TEXT NOT NULL DEFAULT 'auto';

ALTER TABLE products ADD COLUMN IF NOT EXISTS default_pr_gate_mode TEXT NOT NULL DEFAULT 'auto';

-- Track pending changes when PR gate is manual and implementation is complete
CREATE TABLE IF NOT EXISTS pending_pr_changes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID NOT NULL REFERENCES work_items(id),
    repository_id UUID NOT NULL,
    branch TEXT NOT NULL,
    base_branch TEXT NOT NULL DEFAULT 'main',
    files_changed TEXT[] DEFAULT '{}',
    diff_stats TEXT DEFAULT '',
    pr_title TEXT DEFAULT '',
    pr_description TEXT DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending', -- pending, approved, rejected, expired
    approved_by UUID,
    approved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_pending_pr_changes_work_item ON pending_pr_changes(work_item_id);
CREATE INDEX IF NOT EXISTS idx_pending_pr_changes_status ON pending_pr_changes(status);
