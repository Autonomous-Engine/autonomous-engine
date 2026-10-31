package tools

import (
	"encoding/json"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// Tool description style guide (applied 2026-04-23 after auditing a
// well-tuned coding agent's prompts):
//
//  - Open with a one-line capability statement.
//  - Follow with "Usage:" bullets that state CONSTRAINTS, not explanations.
//  - Embed negative examples ("do NOT …") inline — smaller models attend
//    to them better at tool-schema level than at system-prompt level.
//  - Cross-reference sibling tools when there's a common failure mode
//    that picking a different tool would solve.
//  - Keep each description under ~200 tokens. Descriptions longer than
//    the model's attention for a tool call get skimmed.

// ReadFile returns a tool definition for reading a file from the repo.
func ReadFile() adapter.Tool {
	return adapter.Tool{
		Name: "read_file",
		Description: "Read a repository file. Without a range it returns up to 400 lines (4,800 chars) and, for a longer file, " +
			"a hint naming the next range. Read a path before edit_file or write_file on it (a \"no such file\" " +
			"result counts); a missing file means search for the real one, not invent a path.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the repo root"},"start_line":{"type":"integer","description":"1-based first line"},"end_line":{"type":"integer","description":"1-based last line, inclusive"}},"required":["path"]}`),
	}
}

// WriteFile returns a tool definition for writing a file in the repo.
func WriteFile() adapter.Tool {
	return adapter.Tool{
		Name: "write_file",
		Description: "Create a file, or replace a whole file, with content: the COMPLETE file, never a fragment or " +
			"\"// ... rest of file\". Use edit_file to change an existing file. Read the path first. " +
			"A write with syntax errors is reverted and the errors are returned.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the repo root"},"content":{"type":"string","description":"The complete file content"}},"required":["path","content"]}`),
	}
}

// EditFile returns a tool definition for making targeted edits to an existing file.
func EditFile() adapter.Tool {
	return adapter.Tool{
		Name: "edit_file",
		Description: "Replace one exact occurrence of old_string with new_string in an existing file. Read the file " +
			"first and copy old_string verbatim (whitespace included) with enough context to match exactly once; " +
			"several matches return their locations (near_line picks one). new_string replaces old_string, it must not repeat it. " +
			"An edit with syntax errors is reverted and the errors are returned.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the repo root"},"old_string":{"type":"string","description":"Exact current text"},"new_string":{"type":"string","description":"Replacement text"},"near_line":{"type":"integer","description":"Of several matches, the one nearest this line"}},"required":["path","old_string","new_string"]}`),
	}
}

// SearchCode returns a tool definition for searching patterns across the codebase.
func SearchCode() adapter.Tool {
	return adapter.Tool{
		Name: "search_code",
		Description: `Search for a text pattern or regex across all source files in the repository. Returns matching file paths with line numbers.

Usage:
  - Prefer codex_find_module / codex_workspace_symbols / codex_similar_code BEFORE falling back to search_code. The codex is a pre-computed knowledge graph and returns structural answers; search_code is a raw grep and returns noise you have to sift through.
  - pattern supports regex. Anchor tight patterns ("\\bContactForm\\b") to avoid 500-line result sets on common words.
  - file_glob narrows scope (e.g. "*.ts", "app/**/*.tsx", "!vendor/**"). Always set it when you know the language you care about.
  - Use search_code to CONFIRM an existing string before edit_file, or to LOCATE a real file when read_file returned "no such file". Do NOT use it to brainstorm paths.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Search pattern (supports regex)"},"file_glob":{"type":"string","description":"Optional glob pattern to filter files, e.g. *.go, src/**/*.ts"}},"required":["pattern"]}`),
	}
}

// ListFiles returns a tool definition for listing files in a directory.
func ListFiles() adapter.Tool {
	return adapter.Tool{
		Name:        "list_files",
		Description: "List one directory's entries (\"d \" marks a directory). Use it to find a file by name.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory relative to the repo root, '.' for the root"}},"required":["path"]}`),
	}
}

// RunCommand returns a tool definition for executing a shell command.
func RunCommand() adapter.Tool {
	return adapter.Tool{
		Name: "run_command",
		Description: "Run a shell command in the repository root (tests, builds, installs) and return its output and " +
			"exit code. Test-runner output is cut to the failures, the totals and the last lines; raw=true returns " +
			"all of it. Destructive commands (rm -rf on roots, sudo, curl|sh, force-push to main) are rejected. " +
			"Do not edit files with it.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","description":"Default 60"},"raw":{"type":"boolean","description":"Unfiltered output"}},"required":["command"]}`),
	}
}

// GitLog returns a tool definition for viewing recent git history.
func GitLog() adapter.Tool {
	return adapter.Tool{
		Name: "git_log",
		Description: `View recent git commit history. Optionally filter by file path to see a file's history.

Usage:
  - Use path to answer "when/why did X change?" without scanning the whole repo's history.
  - max_commits defaults to 20; raise it only when you genuinely need more context (rare).
  - git_log is for REASONING about prior work. If you need to apply or revert changes, use edit_file / write_file on the current file.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Optional file path to filter history"},"max_commits":{"type":"integer","description":"Max number of commits to return, default 20"}}}`),
	}
}

// GitDiff returns a tool definition for viewing git diffs.
func GitDiff() adapter.Tool {
	return adapter.Tool{
		Name: "git_diff",
		Description: `View git diff. With no arguments shows uncommitted changes in the worktree; with ref shows diff against that ref.

Usage:
  - Call after edit_file / write_file to confirm your changes match intent before declaring done.
  - Pass a ref (e.g. "master", "HEAD~3") to compare against a baseline — useful when reviewing a branch's scope.
  - git_diff is read-only. Do NOT use it to "revert" — edit_file back to the original content, or call run_command "git checkout -- <path>" if you genuinely need a revert.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"Optional git ref to diff against"}}}`),
	}
}

// CreateBranch returns a tool definition for creating a new git branch.
func CreateBranch() adapter.Tool {
	return adapter.Tool{
		Name: "create_branch",
		Description: `Create and checkout a new git branch from the current HEAD.

Usage:
  - Most implementation flows do NOT need this — the agent creates the per-subtask branch deterministically before dispatching you. Only call create_branch if the task explicitly asks you to fork a new branch for a secondary purpose.
  - name must follow the repo's convention (e.g. "feature/…", "fix/…"); avoid spaces.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Branch name"}},"required":["name"]}`),
	}
}

// CommitAndPush returns a tool definition for committing and pushing all changes.
func CommitAndPush() adapter.Tool {
	return adapter.Tool{
		Name: "commit_and_push",
		Description: `Stage all changes, commit with the given message, and push to remote.

Usage:
  - Most implementation flows do NOT need this — the agent handles commit + push deterministically after the tool loop completes, using the diff you produced and a templated message. Call commit_and_push only when the task explicitly needs mid-flow commits (e.g. a multi-commit sequence).
  - message should follow conventional-commit style ("feat: ...", "fix: ..."). One subject line ≤72 chars, optional body.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Commit message"}},"required":["message"]}`),
	}
}

// CreatePullRequest returns a tool definition for creating a GitHub pull request.
func CreatePullRequest() adapter.Tool {
	return adapter.Tool{
		Name: "create_pull_request",
		Description: `Create a pull request on GitHub from the current branch to the base branch.

Usage:
  - Most flows do NOT need this — the pr-coordinator creates the PR once all subtasks pass. Do not call create_pull_request unless the task explicitly requires a mid-flow PR (very rare).
  - body is markdown; include a short "what changed" summary, not a dump of the diff (the reviewer sees the diff natively).
  - base defaults to "main". For repos that use "master" or another default, the coordinator reads base_branch from the repositories table — you do not need to guess.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"PR title"},"body":{"type":"string","description":"PR description in markdown"},"base":{"type":"string","description":"Base branch, default 'main'"}},"required":["title","body"]}`),
	}
}

// RequestUserInput returns a tool that pauses the work item for human input.
// The tool publishes a Kafka event that the unified-interaction-surface
// consumer in dashboard-backend persists and surfaces in the bell / sheet /
// approvals UI. The work item transitions to 'awaiting_user' via
// status-orchestrator and the agent yields — Task 19 will wire the hard
// tool-loop bailout; for this milestone the tool returns a user-visible
// message that the LLM will treat as a stop signal.
func RequestUserInput() adapter.Tool {
	return adapter.Tool{
		Name: "request_user_input",
		Description: `Pause the work item and ask the user a question. Call this when:
  - A codex/qdrant lookup contradicts your plan (e.g., codex says a function the plan assumes doesn't exist).
  - The last 3 tool-call attempts produced no new information (stuck).
  - The user's intent is genuinely ambiguous and guessing would edit the wrong file or function.

BEFORE calling, query past_decisions_query first — the user may have already answered something similar on an earlier ticket.

Usage:
  - headline: one-line summary of what you're stuck on (shown in the notification bell).
  - question: full body with context and candidate options; support markdown.
  - actions: array of button specs {id, label, semantic}. Semantic must be one of retry / restart / continue / cancel / stop / ok / custom.
  - context_payload: attached evidence (failed tool calls, candidate files/functions, diffs). Shown in the detail pane.
  - prompt_type: MUST be 'clarification' | 'escalation' | 'block_question'. Do NOT use 'approval' — approval prompts are published by product-agent only.

CRITICAL: After calling this tool, STOP emitting further tool calls this turn. The work item is paused until the user answers. You will be resumed with the user's answer injected as a user-role message.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"headline":        {"type": "string", "description": "One-line summary"},
				"question":        {"type": "string", "description": "Full question body, markdown allowed"},
				"actions":         {"type": "array", "items": {"type": "object", "properties": {"id": {"type": "string"}, "label": {"type": "string"}, "semantic": {"type": "string", "enum": ["retry","restart","continue","cancel","stop","ok","custom"]}}, "required": ["id","label","semantic"]}},
				"context_payload": {"type": "object", "description": "Attached evidence"},
				"prompt_type":     {"type": "string", "enum": ["clarification","escalation","block_question"]}
			},
			"required": ["headline","question","prompt_type"]
		}`),
	}
}

// PastDecisionsQuery returns a tool that semantically searches prior user
// answers to agent prompts on this product (or this work item). Use BEFORE
// request_user_input — a similar question may have already been answered.
func PastDecisionsQuery() adapter.Tool {
	return adapter.Tool{
		Name: "past_decisions_query",
		Description: `Search prior user decisions recorded from past interaction prompts. Use this BEFORE request_user_input to check whether the user has already answered a similar question on another ticket.

Usage:
  - query: natural-language description of the decision you want to find. Example: "how should we name IDs" or "which file handles email sending".
  - scope: MUST be 'work_item' (only this ticket's prior exchanges) or 'product' (every ticket on the current product). 'global' is NOT supported in v1.
  - limit: 1-50 (default 10).

Returns a JSON array of prior decisions with question, answer, classified_as, source_work_item_id, answered_at. Revoked decisions are excluded automatically.

CRITICAL: Use this BEFORE asking a new question via request_user_input. If a past decision clearly answers yours, adopt it and do NOT re-ask.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Natural-language description"},
				"scope": {"type": "string", "enum": ["work_item","product"]},
				"limit": {"type": "integer", "minimum": 1, "maximum": 50}
			},
			"required": ["query","scope"]
		}`),
	}
}

// ImplementationTools returns the complete toolkit for implementation agents.
func ImplementationTools() []adapter.Tool {
	return []adapter.Tool{
		ReadFile(), WriteFile(), SearchCode(), ListFiles(),
		RunCommand(), GitLog(), GitDiff(), CreateBranch(),
		CommitAndPush(), CreatePullRequest(), RequestUserInput(), PastDecisionsQuery(), RecallContext(),
	}
}

// SafeImplementationTools returns tools for implementation agents without git operations.
// Git operations (branch, commit, push, PR) are handled deterministically by the agent,
// not by the LLM. This prevents the LLM from creating random branches or committing prematurely.
// Codex tools are included so the agent can query the project's knowledge graph before
// falling back to keyword search.
func SafeImplementationTools() []adapter.Tool {
	base := []adapter.Tool{ReadFile(), EditFile(), InsertAfter(), AppendToFile(), WriteFile(), SearchCode(), ListFiles(), RunCommand(), GitLog(), GitDiff(), RequestUserInput(), PastDecisionsQuery(), RecallContext()}
	return append(base, CodexTools()...)
}

// ResearchTools returns the union of tools any read-only research caller may
// use. The research-agent itself sends the scoped ResearchToolsFor set.
func ResearchTools() []adapter.Tool {
	base := []adapter.Tool{ReadFile(), SearchCode(), ListFiles(), GitLog(), GitDiff(), RequestUserInput(), PastDecisionsQuery(), RecallContext()}
	return append(base, CodexTools()...)
}

// ReviewTools returns tools for code review agents.
// Codex tools let the reviewer check whether a change matches established
// patterns or violates known anti-patterns without having to grep blindly.
func ReviewTools() []adapter.Tool {
	base := []adapter.Tool{ReadFile(), SearchCode(), ListFiles(), GitDiff(), RequestUserInput(), PastDecisionsQuery(), RecallContext()}
	return append(base, CodexTools()...)
}

// QATools returns tools for QA agents that need to run tests.
func QATools() []adapter.Tool {
	return []adapter.Tool{ReadFile(), SearchCode(), ListFiles(), RunCommand(), GitDiff(), RequestUserInput(), PastDecisionsQuery(), RecallContext()}
}

// RecallContext returns a tool definition for retrieving a conversation
// checkpoint stored during episodic memory compaction.
func RecallContext() adapter.Tool {
	return adapter.Tool{
		Name: "recall_context",
		Description: `Retrieve the full conversation history from a previous checkpoint.

Usage:
  - When context has been checkpointed, you will see a summary with a snapshot ID.
  - Call this tool with that snapshot_id to retrieve the complete tool call history from before the checkpoint.
  - Only call this when the summary does not contain enough detail for your current task — most of the time the summary is sufficient.`,
		Parameters: json.RawMessage(`{"type":"object","properties":{"snapshot_id":{"type":"string","description":"The UUID of the conversation checkpoint to retrieve"}},"required":["snapshot_id"]}`),
	}
}

// CreateWorkItem returns a tool definition for creating a new work item
// in the pipeline. Used by the chat-agent so users can request changes
// conversationally.
// ListProducts returns a tool that lists all configured products so the AI
// can discover product IDs for linking work items.
func ListProducts() adapter.Tool {
	return adapter.Tool{
		Name:        "list_products",
		Description: `List all configured products in the system. Returns each product's ID, name, and description. Call this BEFORE create_work_item so you can set the correct product_id. Every work item should be linked to a product.`,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func CreateWorkItem() adapter.Tool {
	return adapter.Tool{
		Name: "create_work_item",
		Description: `Create a new work item (ticket) in the pipeline. Use this when the user reports a bug, requests a feature, or needs a code change. The work item goes through the full pipeline (PRD, research, implementation, review, QA).

Usage:
  - ALWAYS call list_products first to find the correct product_id.
  - product_id: UUID of the product this work item belongs to. Required for the item to be picked up by agents.
  - title: short, imperative description of the work item.
  - description: detailed description including acceptance criteria. Be specific.
  - type: "bug" for defects/broken behavior, "feature" for new functionality, "task" for maintenance/chores.
  - priority: "critical" for production-breaking, "high" for serious issues, "medium" for normal work, "low" for nice-to-haves.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title":       {"type": "string", "description": "Short title for the work item"},
				"description": {"type": "string", "description": "Detailed description including acceptance criteria"},
				"product_id":  {"type": "string", "description": "UUID of the product (call list_products to find this)"},
				"type":        {"type": "string", "enum": ["feature", "bug", "task"], "description": "Work item type"},
				"priority":    {"type": "string", "enum": ["low", "medium", "high", "critical"], "description": "Priority level"}
			},
			"required": ["title", "description", "product_id"]
		}`),
	}
}

func ProposeWorkItemChange() adapter.Tool {
	return adapter.Tool{
		Name: "propose_work_item_change",
		Description: `Propose a change to an existing work item. Use this when the user asks to modify
a work item's acceptance criteria, description, priority, or to pause/resume it.
The proposal will be shown to the user for approval before any changes are made.

IMPORTANT: Never modify a work item directly. Always propose the change first.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"work_item_id": {"type": "string", "description": "UUID of the work item to modify"},
				"change_type":  {"type": "string", "enum": ["update_criteria", "update_description", "add_context", "change_priority", "pause", "resume"], "description": "Type of change"},
				"changes":      {"type": "object", "description": "Change details: { old: string, new: string, rationale: string }"}
			},
			"required": ["work_item_id", "change_type", "changes"]
		}`),
	}
}

func ExecuteApprovedProposal() adapter.Tool {
	return adapter.Tool{
		Name: "execute_approved_proposal",
		Description: `Execute a proposal that the user has approved. Only call this after receiving
confirmation that the proposal was approved. The message_id identifies which
proposal to execute.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"message_id": {"type": "string", "description": "UUID of the chat message containing the approved proposal"}
			},
			"required": ["message_id"]
		}`),
	}
}

func GetWorkItemDetail() adapter.Tool {
	return adapter.Tool{
		Name: "get_work_item_detail",
		Description: `Get full details of a work item including title, description, acceptance criteria,
current state, priority, and recent activity. Use this to understand the current
state of a work item before proposing changes.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"work_item_id": {"type": "string", "description": "UUID of the work item"}
			},
			"required": ["work_item_id"]
		}`),
	}
}

func GetBoardSummary() adapter.Tool {
	return adapter.Tool{
		Name: "get_board_summary",
		Description: `Get a summary of the pipeline board: counts by state, blocked items with reasons,
recent activity. Use this when the user asks about overall pipeline status or
asks questions like "why are tickets blocked?"`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"status_filter":   {"type": "string", "description": "Optional: filter by state (e.g. 'blocked', 'implementing')"},
				"priority_filter": {"type": "string", "description": "Optional: filter by priority (e.g. 'critical', 'high')"}
			}
		}`),
	}
}

func GetSessionHistory() adapter.Tool {
	return adapter.Tool{
		Name: "get_session_history",
		Description: `Get a summary of prior messages in the current or a specified chat session.
Use this for context continuity when resuming a session.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "UUID of the session (defaults to current session if omitted)"}
			}
		}`),
	}
}

// GetSystemInfo returns a tool that reports the current model, provider, and agent identity.
func GetSystemInfo() adapter.Tool {
	return adapter.Tool{
		Name:        "get_system_info",
		Description: `Returns the current LLM provider, model name, and agent identity. Call this when you need to know which model you are or what provider is serving you.`,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func SetProductContext() adapter.Tool {
	return adapter.Tool{
		Name: "set_product_context",
		Description: `Set the active product for this chat session. This scopes codex tools (codex_find_module, codex_similar_code, etc.) to the selected product's codebase.

IMPORTANT RULES:
- NEVER call this tool on your own. The user MUST explicitly tell you which product they want.
- When the user mentions something that could relate to multiple products, call list_products and present the options. Wait for the user to choose.
- Only call this after the user has clearly identified or confirmed the product.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"product_id": {"type": "string", "description": "UUID of the product to set as context (from list_products)"}
			},
			"required": ["product_id"]
		}`),
	}
}

// ChatTools returns tools for the interactive chat agent. Read-only code
// access + codex + work item creation + work item modification. No write tools.
func ChatTools() []adapter.Tool {
	base := []adapter.Tool{
		ReadFile(), SearchCode(), ListFiles(), PastDecisionsQuery(),
		ListProducts(), CreateWorkItem(), SetProductContext(),
		ProposeWorkItemChange(), ExecuteApprovedProposal(),
		GetWorkItemDetail(), GetBoardSummary(), GetSessionHistory(),
		GetSystemInfo(),
	}
	return append(base, CodexTools()...)
}
