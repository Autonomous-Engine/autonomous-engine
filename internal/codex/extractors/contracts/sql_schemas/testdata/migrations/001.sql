CREATE TABLE users (id UUID, name TEXT);
ALTER TABLE users ADD COLUMN email TEXT;
CREATE TABLE orders (id UUID, user_id UUID, total NUMERIC);
