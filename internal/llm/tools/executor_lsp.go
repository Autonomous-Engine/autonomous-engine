package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
)

// ToolOverride answers a tool call in place of the executor's own handler
// when it returns true. It exists for offline harnesses (replays and
// fake-model tests) that need realistic results without live services.
type ToolOverride func(ctx context.Context, call adapter.ToolCall) (adapter.ToolResult, bool)

// SetToolOverride installs o; nil removes it.
func (e *Executor) SetToolOverride(o ToolOverride) { e.toolOverride = o }

// SetCodexReadsFromWorktree makes codex_read_file, codex_grep,
// codex_list_tree and codex_verify_imports read the executor's own tree
// for the default repository instead of the product's repo-cache clone.
// The implementation agent sets it: its executor is rooted at a task
// worktree whose edits the clone never sees, so those tools returned
// pre-edit content after the agent's first edit.
func (e *Executor) SetCodexReadsFromWorktree(on bool) { e.codexWorktreeReads = on }

// SetWorktreeRepo names the repository the executor's tree is a worktree of
// (the task's repository). With worktree reads on, repo=<name> (or no repo)
// reads the worktree; any other repository reads its repo-cache clone.
func (e *Executor) SetWorktreeRepo(name string) { e.worktreeRepo = strings.TrimSpace(name) }

// codexReadRoot is the root the codex read tools resolve repoName against:
// the executor's tree for the task's repository (SetWorktreeRepo; no repo
// given means that one) when worktree reads are on, else the repo-cache
// clone (repoCacheRoot).
func (e *Executor) codexReadRoot(ctx context.Context, repoName string) (string, string, error) {
	if e.codexWorktreeReads && e.repoPath != "" {
		name := e.worktreeRepo
		if name == "" {
			name = filepath.Base(e.repoPath)
		}
		if r := strings.TrimSpace(repoName); r == "" || r == name {
			return e.repoPath, name, nil
		}
	}
	return e.repoCacheRoot(ctx, repoName)
}

// codexReadsAvailable reports whether the codex read tools can answer.
func (e *Executor) codexReadsAvailable() bool {
	return e.codexWorktreeReads || (e.codex != nil && e.productID != nil)
}

// currentLines returns lines start..end (1-based, inclusive) of a file in
// the executor's tree as it is now; false when it cannot be read. A pinned
// read (LoopOptions.PinReads) shows this, so the model sees its own edits.
func (e *Executor) currentLines(rel string, start, end int) (string, bool) {
	full := filepath.Join(e.repoPath, filepath.FromSlash(rel))
	// Rel, not a string prefix: a sibling such as repo-x is outside repo.
	if r, err := filepath.Rel(filepath.Clean(e.repoPath), full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if start < 1 {
		start = 1
	}
	if end < start || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", true
	}
	return strings.Join(lines[start-1:end], "\n"), true
}

// SetLSPManager replaces the executor's LSP manager. Agents that build one
// executor per task share a process-wide manager this way, so a warm
// language server and a cached readiness failure outlive one task.
func (e *Executor) SetLSPManager(m *lsp.Manager) { e.lspManager = m }

// lspRoot is the workspace the LSP tools open: the default repository's
// repo-cache root (the same root codexLSPDispatch resolves). A successful
// resolution is kept for the executor's lifetime; a failed one (a
// transient DB error) is retried on the next call, so it cannot hide the
// LSP tools for the rest of the task.
func (e *Executor) lspRoot(ctx context.Context) (string, bool) {
	e.lspRootMu.Lock()
	defer e.lspRootMu.Unlock()
	if e.lspRootResolved {
		return e.lspRootPath, true
	}
	resolve := e.lspRootResolver
	if resolve == nil {
		resolve = func(ctx context.Context) (string, error) {
			root, _, err := e.repoCacheRoot(ctx, "")
			return root, err
		}
	}
	root, err := resolve(ctx)
	if err != nil || root == "" {
		return "", false
	}
	e.lspRootPath, e.lspRootResolved = root, true
	return root, true
}

// LSPReady reports, without blocking on a language server, whether the LSP
// tools would answer for this executor's workspace right now.
func (e *Executor) LSPReady(ctx context.Context) bool {
	if e.lspManager == nil || e.codex == nil || e.productID == nil {
		return false
	}
	root, ok := e.lspRoot(ctx)
	if !ok {
		return false
	}
	return e.lspManager.Ready(root)
}

// WarmLSP starts the workspace's language server unless it is ready, being
// probed, or inside a failure cooldown. It blocks for the probe: run it in
// a goroutine with a context that outlives one LLM call.
func (e *Executor) WarmLSP(ctx context.Context) {
	if e.lspManager == nil || e.codex == nil || e.productID == nil {
		return
	}
	root, ok := e.lspRoot(ctx)
	if !ok {
		return
	}
	e.lspManager.Warm(ctx, root)
}
