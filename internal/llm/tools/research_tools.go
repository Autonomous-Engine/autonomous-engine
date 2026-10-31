package tools

import "github.com/autonomousengine/autonomous-engine/internal/llm/adapter"

// ResearchToolOptions tunes the research tool set to what can actually
// answer during this task.
type ResearchToolOptions struct {
	// LSPReady adds the six language-server tools. Leave it false unless
	// the workspace's language server is known to be ready
	// (lsp.Manager.Ready); a not-ready server fails every call.
	LSPReady bool
}

// ResearchToolsFor returns the tool set one research task is offered. Every
// schema is re-sent on every call of the task, so the set holds only tools
// that can answer: the codex graph, search and read tools; git history for
// the history-oriented types; the LSP tools only when the server is ready.
// Never offered: request_user_input (research cannot ask), past_decisions_query
// and recall_context, the raw read_file/search_code/list_files duplicates,
// and the write-side codex_verify_imports / codex_contract_edges.
func ResearchToolsFor(researchType string, opts ResearchToolOptions) []adapter.Tool {
	out := []adapter.Tool{
		CodexFindModule(),
		CodexDependentsOf(),
		CodexDependenciesOf(),
		CodexSimilarCode(),
		CodexGrep(),
		CodexReadFile(),
		CodexListTree(),
		CodexPatternsFor(),
	}
	switch researchType {
	case "history_analysis":
		out = append(out, GitLog())
	case "bug_archaeology":
		out = append(out, GitLog(), GitDiff())
	}
	if opts.LSPReady {
		out = append(out, CodexLSPTools()...)
	}
	return out
}
