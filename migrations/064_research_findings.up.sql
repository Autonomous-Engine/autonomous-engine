-- Research findings: one verified claim + citation per row, recorded by the
-- research tool loop (record_finding) and recalled by later tasks and rounds
-- of the same work item.
CREATE TABLE IF NOT EXISTS research_findings (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id   UUID NOT NULL,
    task_id        TEXT NOT NULL,
    research_type  TEXT NOT NULL DEFAULT '',
    product_id     UUID,
    claim          TEXT NOT NULL,
    path           TEXT NOT NULL,
    start_line     INTEGER NOT NULL,
    end_line       INTEGER NOT NULL,
    excerpt        TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE research_findings DROP CONSTRAINT IF EXISTS research_findings_work_item_id_fkey;
ALTER TABLE research_findings ADD CONSTRAINT research_findings_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE research_findings DROP CONSTRAINT IF EXISTS research_findings_product_id_fkey;
ALTER TABLE research_findings ADD CONSTRAINT research_findings_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_research_findings_work_item_created
    ON research_findings (work_item_id, created_at DESC);
