-- Adds a human-in-the-loop approval gate between PRD generation and research
-- dispatch. Before this change product-agent published prd.ready the moment
-- the LLM returned, and research-coordinator immediately dispatched five
-- research tasks — so a hallucinated PRD would eat 30+ minutes of expensive
-- LLM time investigating a fictional feature. With this state, product-agent
-- parks the work item in `prd_approval` and only an admin clicking Approve
-- promotes it to `research`.
--
-- This is the principle "no hallucinated success" enforced structurally,
-- not by heuristics. The previous approach tried a strings.Contains check
-- on the LLM's response for "ambiguous" / "unclear" — which both misfired
-- (false positives on "not unclear") and dead-ended into an unsubscribed
-- Kafka topic. The correct answer is a state and a human.

ALTER TYPE work_item_state ADD VALUE IF NOT EXISTS 'prd_approval' AFTER 'prd';
