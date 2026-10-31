DROP TABLE IF EXISTS pending_pr_changes;
ALTER TABLE products DROP COLUMN IF EXISTS default_pr_gate_mode;
ALTER TABLE work_items DROP COLUMN IF EXISTS pr_gate_mode;
