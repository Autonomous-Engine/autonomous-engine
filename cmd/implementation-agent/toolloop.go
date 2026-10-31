package main

import (
	"context"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lspToolNames are the language-server tools; they answer only once the
// workspace's server is ready.
var lspToolNames = func() map[string]bool {
	out := map[string]bool{}
	for _, t := range tools.CodexLSPTools() {
		out[t.Name] = true
	}
	return out
}()

// withoutLSPUnlessReady is base, minus the LSP tools while the server is
// not ready (a live run, attempt 6: 15 codex_workspace_symbols calls, all
// "workspace not ready").
func withoutLSPUnlessReady(base []adapter.Tool, ready bool) []adapter.Tool {
	if ready {
		return base
	}
	out := make([]adapter.Tool, 0, len(base))
	for _, t := range base {
		if !lspToolNames[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// editTools is the main loop's tool set (T1): the edit phase with the LSP
// tools, which RefreshTools withholds until the server is ready.
func editTools() []adapter.Tool {
	return tools.ImplementationToolsFor(tools.ImplementationPhaseEdit, tools.ImplementationToolOptions{LSPReady: true})
}

// verifyTools is the repair loop's tool set (T1): read, run, edit, write,
// grep.
func verifyTools() []adapter.Tool {
	return tools.ImplementationToolsFor(tools.ImplementationPhaseVerify, tools.ImplementationToolOptions{LSPReady: true})
}

// implementationKeepRecent is how many tool-call iterations stay in full;
// older results become ledger lines (T4).
const implementationKeepRecent = 2

// implementationLoopOptions are the loop options of every implementation
// tool loop: the dead-tool guard (a tool failing "not ready / unavailable"
// twice in a row is dropped, within a turn too) and a tool list re-read
// every iteration so the LSP tools appear once the language server is
// ready (B5); the evidence ledger with the last 2 iterations in full and
// pinned reads — the latest read of a file in scope or edited stays in
// full, as the file is now (T4); a re-read of lines it still has,
// unchanged, answered in one line (R12 X2).
func implementationLoopOptions(base []adapter.Tool, lspReady func() bool, scope []string) tools.LoopOptions {
	base = append([]adapter.Tool(nil), base...)
	return tools.LoopOptions{
		EvidenceLedger: true,
		KeepRecent:     implementationKeepRecent,
		PinReads:       true,
		PinnedFiles:    append([]string(nil), scope...),
		DedupReads:     true,
		DeadToolGuard:  true,
		StuckControl:   true,
		RefreshTools: func() []adapter.Tool {
			return withoutLSPUnlessReady(base, lspReady())
		},
	}
}

// runImplementationLoop runs one implementation tool loop with
// implementationLoopOptions over req's tools; scope is the task's files in
// scope (pinned once read).
func runImplementationLoop(ctx context.Context, llm adapter.ModelAdapter, req adapter.CompletionRequest, executor *tools.Executor, maxIters int, pool *pgxpool.Pool, workID *uuid.UUID, scope []string) (*adapter.CompletionResponse, error) {
	return runImplementationLoopAfter(ctx, llm, req, executor, maxIters, pool, workID, scope, nil)
}

// runImplementationLoopAfter is runImplementationLoop for a pass after
// one that ended stuck (R10 W4): a repeat of a prior stuck call ends it.
func runImplementationLoopAfter(ctx context.Context, llm adapter.ModelAdapter, req adapter.CompletionRequest, executor *tools.Executor, maxIters int, pool *pgxpool.Pool, workID *uuid.UUID, scope []string, prior []tools.LoopStuck) (*adapter.CompletionResponse, error) {
	opts := implementationLoopOptions(req.Tools, func() bool { return executor.LSPReady(ctx) }, scope)
	opts.PriorStuck = prior
	req.Tools = opts.RefreshTools()
	return tools.RunToolLoopWithOptions(ctx, llm, req, executor, maxIters, pool, workID, opts)
}
