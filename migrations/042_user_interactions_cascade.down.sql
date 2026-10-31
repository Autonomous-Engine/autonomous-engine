ALTER TABLE user_interactions DROP CONSTRAINT IF EXISTS user_interactions_product_id_fkey;
ALTER TABLE user_interactions ADD CONSTRAINT user_interactions_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id);
