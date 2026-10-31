package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func names(ts []adapter.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// editPhaseSchemaCeiling is T1's budget for the edit-phase schemas, which
// are re-sent on every call (attempt 6: 27 tools, 23,401 chars).
const editPhaseSchemaCeiling = 9000

// T1: the implementation loops get a scoped tool set per phase.
func TestImplementationToolsForPhases(t *testing.T) {
	edit := ImplementationToolsFor("edit", ImplementationToolOptions{})
	wantEdit := []string{"append_to_file", "codex_dependents_of", "codex_find_module", "codex_grep", "codex_read_file", "codex_similar_code",
		"codex_verify_imports", "edit_file", "insert_after", "list_files", "read_file", "run_command", "write_file"}
	if got := names(edit); strings.Join(got, ",") != strings.Join(wantEdit, ",") {
		t.Errorf("edit phase = %v\nwant %v", got, wantEdit)
	}
	b, _ := json.Marshal(edit)
	t.Logf("edit-phase schemas: %d tools, %d JSON chars (before: SafeImplementationTools, 27 tools)", len(edit), len(b))
	if len(b) > editPhaseSchemaCeiling {
		t.Errorf("edit-phase schemas are %d JSON chars, want <= %d", len(b), editPhaseSchemaCeiling)
	}

	verify := ImplementationToolsFor("verify", ImplementationToolOptions{})
	if got := names(verify); strings.Join(got, ",") != "append_to_file,codex_grep,edit_file,insert_after,read_file,run_command,write_file" {
		t.Errorf("verify phase = %v", got)
	}

	// The LSP tools only when the server is ready, in both phases.
	for _, phase := range []string{"edit", "verify"} {
		without := map[string]bool{}
		for _, n := range names(ImplementationToolsFor(phase, ImplementationToolOptions{})) {
			without[n] = true
		}
		with := names(ImplementationToolsFor(phase, ImplementationToolOptions{LSPReady: true}))
		for _, l := range CodexLSPTools() {
			if without[l.Name] {
				t.Errorf("%s: %s offered while the server is not ready", phase, l.Name)
			}
			found := false
			for _, n := range with {
				found = found || n == l.Name
			}
			if !found {
				t.Errorf("%s: %s missing once the server is ready", phase, l.Name)
			}
		}
	}

	// The unions other callers use still hold every scoped tool.
	union := map[string]bool{}
	for _, n := range names(SafeImplementationTools()) {
		union[n] = true
	}
	for _, n := range append(names(edit), names(verify)...) {
		if !union[n] {
			t.Errorf("SafeImplementationTools lacks %s", n)
		}
	}
}

// codex_read_file / codex_grep / codex_list_tree / codex_verify_imports read
// the repo-cache clone; an implementation worktree's edits are not there,
// so after an edit they returned stale content. With worktree reads on they
// read the executor's own tree.
func TestCodexReadsFollowTheWorktree(t *testing.T) {
	root := writeTree(t, map[string]string{"src/env.ts": "export const config = { searchApiKey: '' };\n"})
	ex := NewExecutor(root)
	if r := ex.Execute(context.Background(), adapter.ToolCall{ID: "1", Name: "codex_read_file", Arguments: `{"path":"src/env.ts"}`}); !r.IsError {
		t.Fatalf("without worktree reads and without a codex, codex_read_file answered: %.80q", r.Content)
	}
	ex.SetCodexReadsFromWorktree(true)
	if err := os.WriteFile(filepath.Join(root, "src", "env.ts"), []byte("export const config = { searchApiKey: process.env.SEARCH_API_KEY };\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "2", Name: "codex_read_file", Arguments: `{"path":"src/env.ts"}`})
	if r.IsError || !strings.Contains(r.Content, "process.env.SEARCH_API_KEY") {
		t.Fatalf("codex_read_file: %v %.200q", r.IsError, r.Content)
	}
	g := ex.Execute(context.Background(), adapter.ToolCall{ID: "3", Name: "codex_grep", Arguments: `{"pattern":"SEARCH_API_KEY"}`})
	if g.IsError || !strings.Contains(g.Content, `"file": "src/env.ts"`) {
		t.Fatalf("codex_grep: %v %.200q", g.IsError, g.Content)
	}
	if l := ex.Execute(context.Background(), adapter.ToolCall{ID: "4", Name: "codex_list_tree", Arguments: `{}`}); l.IsError || !strings.Contains(l.Content, "env.ts") {
		t.Fatalf("codex_list_tree: %v %.200q", l.IsError, l.Content)
	}
	// R6 review: the worktree is labelled with the task's repository, not
	// the product's first one: repo=<task repo> reads it, another repo
	// does not.
	ex.SetWorktreeRepo("sample-app")
	if r := ex.Execute(context.Background(), adapter.ToolCall{ID: "6", Name: "codex_read_file", Arguments: `{"path":"src/env.ts","repo":"sample-app"}`}); r.IsError || !strings.Contains(r.Content, `"repo": "sample-app"`) || !strings.Contains(r.Content, "SEARCH_API_KEY") {
		t.Fatalf("repo=task repo: %v %.200q", r.IsError, r.Content)
	}
	if r := ex.Execute(context.Background(), adapter.ToolCall{ID: "7", Name: "codex_read_file", Arguments: `{"path":"src/env.ts","repo":"sample-app-landing"}`}); !r.IsError {
		t.Fatalf("another repository read the worktree: %.200q", r.Content)
	}
	if v := ex.Execute(context.Background(), adapter.ToolCall{ID: "5", Name: "codex_verify_imports", Arguments: `{"path":"src/new.ts","content":"import { config } from './env';\n"}`}); v.IsError || !strings.Contains(v.Content, `"unresolved": []`) && !strings.Contains(v.Content, `"unresolved": null`) {
		t.Fatalf("codex_verify_imports: %v %.300q", v.IsError, v.Content)
	}
}
