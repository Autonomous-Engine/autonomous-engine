-- Full-transparency LLM observability: store the actual conversation that each
-- inference call used, so the dashboard can render prompt + response for every
-- row in LLM Activity. Without this, a customer reviewing what happened only
-- sees token counts — that's accounting, not transparency.
--
-- Stored per-row:
--   prompt_messages   JSONB      — ordered chat messages sent to the model
--                                  (role + content, matching adapter.ChatMessage)
--   response_content  TEXT       — the model's final reply (already shaped for
--                                  display; strategy may have done self-review
--                                  and this is the post-review text)
--
-- Both columns default to empty/null so existing rows keep working. Prompts
-- can be large (tens of KB); that's expected and bounded by context window.

ALTER TABLE inference_logs
    ADD COLUMN IF NOT EXISTS prompt_messages JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS response_content TEXT NOT NULL DEFAULT '';
