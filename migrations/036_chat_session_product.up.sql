-- Add product_id to chat_sessions so the chat agent can scope codex queries
-- to the correct product without needing a linked work item.
ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS product_id UUID REFERENCES products(id) ON DELETE SET NULL;
