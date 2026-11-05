-- A new work item takes its product's default_pr_gate_mode. Migration 029
-- added the product column as "default gate mode for new work items in this
-- product" and the dashboard sets it, but no creation path read it: every
-- new item was 'auto'. The column stays NOT NULL; its own default goes, and
-- a row inserted without a gate is filled from its product (or 'auto' when
-- it has none). An insert that names the gate keeps it.
ALTER TABLE work_items ALTER COLUMN pr_gate_mode DROP DEFAULT;

CREATE OR REPLACE FUNCTION work_items_default_pr_gate_mode() RETURNS trigger AS $$
BEGIN
    IF NEW.pr_gate_mode IS NULL THEN
        IF NEW.product_id IS NOT NULL THEN
            SELECT default_pr_gate_mode INTO NEW.pr_gate_mode
            FROM products WHERE id = NEW.product_id;
        END IF;
        NEW.pr_gate_mode := COALESCE(NEW.pr_gate_mode, 'auto');
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_items_default_pr_gate_mode ON work_items;
CREATE TRIGGER work_items_default_pr_gate_mode
    BEFORE INSERT ON work_items
    FOR EACH ROW EXECUTE FUNCTION work_items_default_pr_gate_mode();
