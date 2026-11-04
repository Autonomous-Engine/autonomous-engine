-- Prompts raised without a product cannot survive the NOT NULL coming back.
DELETE FROM user_interactions WHERE product_id IS NULL;
ALTER TABLE user_interactions ALTER COLUMN product_id SET NOT NULL;
