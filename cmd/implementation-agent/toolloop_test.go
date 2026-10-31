package main

import (
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

func toolNames(ts []adapter.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[t.Name] = true
	}
	return out
}

// B5: the implementation loops offer the LSP tools only while the language
// server is ready, and run with the dead-tool guard.
func TestImplementationLoopOptionsGateLSPAndGuardDeadTools(t *testing.T) {
	base := tools.SafeImplementationTools()
	ready := false
	opts := implementationLoopOptions(base, func() bool { return ready }, []string{"src/env.ts"})
	if !opts.DeadToolGuard {
		t.Fatal("DeadToolGuard is off")
	}
	// T4: the ledger with pinned reads, the last 2 iterations in full.
	if !opts.EvidenceLedger || !opts.PinReads || opts.KeepRecent != 2 || len(opts.PinnedFiles) != 1 {
		t.Fatalf("ledger options = %+v", opts)
	}
	// R12 X2: a re-read of lines it still has is answered in one line.
	if !opts.DedupReads {
		t.Fatal("DedupReads is off")
	}
	if opts.RefreshTools == nil {
		t.Fatal("no RefreshTools: LSP readiness is never re-read")
	}
	got := toolNames(opts.RefreshTools())
	for _, lspTool := range tools.CodexLSPTools() {
		if got[lspTool.Name] {
			t.Errorf("%s offered while the language server is not ready", lspTool.Name)
		}
	}
	if !got["edit_file"] || !got["run_command"] {
		t.Errorf("non-LSP tools missing: %v", got)
	}
	ready = true
	got = toolNames(opts.RefreshTools())
	if !got["codex_workspace_symbols"] {
		t.Error("LSP tools not offered once the server is ready")
	}
}

// T1: the implementation loops send the scoped sets, not the 27-tool union.
func TestImplementationLoopsSendTheScopedToolSets(t *testing.T) {
	for _, f := range []string{"main.go", "generation.go", "repair.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(src), "\r\n", "\n")
		if strings.Contains(s, "tools.SafeImplementationTools()") {
			t.Errorf("%s still sends SafeImplementationTools", f)
		}
	}
	main, _ := os.ReadFile("main.go")
	if !strings.Contains(string(main), "executor.SetCodexReadsFromWorktree(true)") {
		t.Error("main.go does not make the codex read tools read the worktree")
	}
	if !strings.Contains(string(main), "executor.SetWorktreeRepo(taskRepoName(msgCtx, ps, task.RepositoryID))") {
		t.Error("main.go does not label the worktree with the task's repository")
	}
	edit := toolNames(withoutLSPUnlessReady(editTools(), false))
	if len(edit) != 13 || !edit["codex_verify_imports"] || !edit["insert_after"] || !edit["append_to_file"] || edit["request_user_input"] {
		t.Errorf("edit tools = %v", edit)
	}
	if v := toolNames(withoutLSPUnlessReady(verifyTools(), false)); len(v) != 7 || !v["insert_after"] || !v["append_to_file"] {
		t.Errorf("verify tools = %v", v)
	}
}

// Every implementation tool loop goes through runImplementationLoop, so the
// options above apply to all of them (the tool-less plan-revision turn
// excepted).
func TestImplementationLoopsUseTheGuardedRunner(t *testing.T) {
	for _, f := range []string{"generation.go", "repair.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(src), "\r\n", "\n")
		n := strings.Count(s, "tools.RunToolLoop(")
		allowed := 0
		if f == "repair.go" {
			allowed = 1 // revisePlan: one turn, no tools
		}
		if n != allowed {
			t.Errorf("%s calls tools.RunToolLoop %d times, want %d (use runImplementationLoop)", f, n, allowed)
		}
	}
}
