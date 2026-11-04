-- A prompt can be raised for a work item that has no product: a chat intake
-- that named no product is blocked by product-agent ("has no product"), and
-- the block must reach Approvals instead of leaving an invisible dead item
-- (NEW-35). The foreign key (ON DELETE CASCADE, migration 042) stands; only
-- the NOT NULL goes. Readers COALESCE a NULL product to the nil UUID.
ALTER TABLE user_interactions ALTER COLUMN product_id DROP NOT NULL;
