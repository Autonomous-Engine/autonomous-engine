package tools

import "github.com/autonomousengine/autonomous-engine/internal/llm/adapter"

// ImplementationToolOptions tunes the implementation tool set to what can
// answer during this task.
type ImplementationToolOptions struct {
	// LSPReady adds the six language-server tools; leave it false unless
	// the workspace's server is ready (a not-ready server fails every call).
	LSPReady bool
}

// Implementation phases for ImplementationToolsFor.
const (
	// ImplementationPhaseEdit is the main loop: read, search, edit, test.
	ImplementationPhaseEdit = "edit"
	// ImplementationPhaseVerify is a fix-up loop after a failing check
	// (the repair loop): read, run, edit, write, grep.
	ImplementationPhaseVerify = "verify"
)

// ImplementationToolsFor returns the tool set one implementation loop is
// offered. Every schema is re-sent on every call: a live run (attempt 6) sent
// SafeImplementationTools (27 tools, 23,401 JSON chars) on 24 calls and
// used 9 of them. Never offered here: request_user_input,
// past_decisions_query, recall_context, git_log / git_diff, search_code,
// codex_dependencies_of, codex_patterns_for, codex_contract_edges, and the
// LSP tools until the server is ready. codex_verify_imports stays in the
// edit phase: the verify gate asks for it on every proposed file.
// SafeImplementationTools / ImplementationTools stay the unions for other
// callers.
func ImplementationToolsFor(phase string, opts ImplementationToolOptions) []adapter.Tool {
	var out []adapter.Tool
	switch phase {
	case ImplementationPhaseVerify:
		// write_file too: a repair may need to create a missing file.
		out = []adapter.Tool{ReadFile(), RunCommand(), EditFile(), InsertAfter(), AppendToFile(), WriteFile(), CodexGrep()}
	default:
		out = []adapter.Tool{
			ReadFile(), CodexReadFile(), CodexGrep(), CodexFindModule(), CodexDependentsOf(),
			CodexSimilarCode(), ListFiles(), EditFile(), InsertAfter(), AppendToFile(), WriteFile(), RunCommand(), CodexVerifyImports(),
		}
	}
	if opts.LSPReady {
		out = append(out, CodexLSPTools()...)
	}
	return out
}
