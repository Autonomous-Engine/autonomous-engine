package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// The live code_trace task read a 240-line blob of charsets.go (10k tokens a
// call), could not cite it under the 60-line finding cap, and re-read it
// until the repeat guard ended the task. A research executor reads in
// smaller windows, and a finding may cite a whole default window.
func TestReadWindowOverrideBoundsReads(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "line %03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetReadWindow(120, 240)
	ctx := context.Background()
	res := ex.Execute(ctx, adapter.ToolCall{ID: "r1", Name: "codex_read_file", Arguments: `{"path":"big.go"}`})
	if res.IsError || !strings.Contains(res.Content, "line 120") || strings.Contains(res.Content, "line 121") || !strings.Contains(res.Content, "start_line=121") {
		t.Fatalf("default window = %+v, want lines 1-120 and a continuation hint", res)
	}
	res = ex.Execute(ctx, adapter.ToolCall{ID: "r2", Name: "codex_read_file", Arguments: `{"path":"big.go","start_line":1,"end_line":300}`})
	if res.IsError || !strings.Contains(res.Content, "line 240") || strings.Contains(res.Content, "line 241") {
		t.Fatalf("explicit range = %+v, want capped at 240 lines", res)
	}
	res = ex.Execute(ctx, adapter.ToolCall{ID: "r3", Name: "read_file", Arguments: `{"path":"big.go"}`})
	if res.IsError || !strings.Contains(res.Content, "line 120") || strings.Contains(res.Content, "line 121") {
		t.Fatalf("read_file default window = %+v, want 120 lines", res)
	}
	// A finding may cite a whole default window.
	ex.markRead("big.go", 1, 120, 300)
	res = ex.Execute(ctx, recordCall("the whole window", "big.go", 1, 120))
	if res.IsError {
		t.Fatalf("finding on 120 lines refused: %s", res.Content)
	}
}

// Recording the same claim with the same citation again does not add a
// finding (pattern_match recorded one claim four times live).
func TestRecordFindingDeduplicates(t *testing.T) {
	ex := findingsExecutor(t)
	ctx := context.Background()
	ex.markRead("a.go", 1, 10, 10)
	first := ex.Execute(ctx, recordCall("a claim", "a.go", 2, 3))
	again := ex.Execute(ctx, recordCall("a claim", "a.go", 2, 3))
	if first.IsError || again.IsError || !strings.Contains(again.Content, "already recorded as #1") {
		t.Fatalf("first=%+v again=%+v", first, again)
	}
	if n := len(ex.Findings()); n != 1 {
		t.Fatalf("findings = %d, want 1", n)
	}
}
