-- work_tokens: what a call actually added — its output plus the input it
-- had not sent before (tool results, nudges, the first prompt). The
-- work-item budget sums this, not input_tokens + output_tokens, which
-- counted the whole re-sent context on every call (94% of the "budget"
-- on a 70-call item was the same prompt being resent). NULL means the
-- call was not a tool-loop call: its input is fresh content, so the
-- budget uses input_tokens + output_tokens.
ALTER TABLE inference_logs ADD COLUMN IF NOT EXISTS work_tokens INTEGER;
