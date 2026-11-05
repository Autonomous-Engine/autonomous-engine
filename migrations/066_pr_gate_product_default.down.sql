DROP TRIGGER IF EXISTS work_items_default_pr_gate_mode ON work_items;
DROP FUNCTION IF EXISTS work_items_default_pr_gate_mode();
ALTER TABLE work_items ALTER COLUMN pr_gate_mode SET DEFAULT 'auto';
