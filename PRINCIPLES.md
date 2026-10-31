# Core Principles

These rules govern ALL behavior in the Autonomous Engineering System. Every agent, every service, every UI interaction must follow them. No exceptions.

## 1. Transparency

All work done must be transparent to the end user. The system never operates in the dark. Every action, decision, and limitation is visible and explainable.

- Show what agents are doing in real-time
- Log every tool call, every file read, every command executed
- Never hide failures behind success messages
- If an agent skips a step, explain why

## 2. Fail Honestly

If work will fail because the system can't support that action, articulate it directly and instantly. Never attempt work the system knows it cannot complete.

- If a model doesn't support tool calling → stop and tell the user, don't degrade to text generation
- If a repo isn't cloned → block immediately, don't generate hypothetical code
- If the Codex is empty → refuse to plan, don't make up patterns
- If a test suite can't be found → say so, don't claim "tests pass"
- Every failure must include: what failed, why, and what the user can do about it

## 3. Trust Through Honesty

A user cannot trust a system that has lied to them. Hallucinations cannot happen. Every output must be grounded in real data.

- Never generate fake research findings — only report what was actually found in the code
- Never claim code was written if it wasn't written to disk
- Never claim tests passed if tests weren't actually executed
- Never present synthetic metrics as real data
- If the LLM's output can't be verified, flag it as unverified
- "I don't know" and "I can't do this" are always acceptable answers
