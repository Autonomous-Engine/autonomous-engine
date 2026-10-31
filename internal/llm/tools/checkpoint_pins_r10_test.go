package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// R10 W6: live-run attempt 9's checkpoint (call 3 -> 4) removed all 9 reads
// before the ledger had pinned them, and the docs edits that followed
// guessed their old_string. Now the checkpoint keeps the newest read of
// every in-scope file in "Current file contents" (the ledger's rule and
// 5,000-char cap).
func TestCheckpointKeepsPinnedInScopeReads(t *testing.T) {
	root := t.TempDir()
	var files []string
	for i := 1; i <= 9; i++ {
		rel := fmt.Sprintf("f%d.md", i)
		files = append(files, rel)
		body := fmt.Sprintf("# File %d\nunique-marker-%d\n", i, i)
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "other.md"), []byte("o1\no2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var reads []adapter.ToolCall
	for i, f := range files {
		reads = append(reads, adapter.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, f)})
	}
	turns := []adapter.CompletionResponse{
		{ToolCalls: reads, FinishReason: "tool_calls", InputTokens: 2500, OutputTokens: 100},
		{ToolCalls: []adapter.ToolCall{{ID: "call_0", Name: "read_file", Arguments: `{"path":"other.md"}`}}, FinishReason: "tool_calls", InputTokens: 3000, OutputTokens: 30},
		// Call 3 reports a prompt over the threshold: the checkpoint fires
		// before the ledger has compacted call 1's reads.
		{ToolCalls: []adapter.ToolCall{{ID: "call_0", Name: "read_file", Arguments: `{"path":"other.md","start_line":1,"end_line":1}`}}, FinishReason: "tool_calls", InputTokens: 70000, OutputTokens: 30},
		{ToolCalls: []adapter.ToolCall{{ID: "call_0", Name: "read_file", Arguments: `{"path":"other.md","start_line":2,"end_line":2}`}}, FinishReason: "tool_calls", InputTokens: 3000, OutputTokens: 30},
	}
	model := &scriptAdapter{turns: turns}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "task"}},
		Tools:    []adapter.Tool{ReadFile(), EditFile()},
	}
	stored := 0
	opts := LoopOptions{
		EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: files,
		CheckpointStore: func(context.Context, uuid.UUID, uuid.UUID, string, int, []byte, string, int) error {
			stored++
			return nil
		},
	}
	ex := NewExecutor(root)
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil, opts); err != nil {
		t.Fatal(err)
	}
	if stored == 0 {
		t.Fatal("the checkpoint did not fire")
	}
	// The request after the checkpoint (call 4) and the last one carry
	// every file's content.
	for _, n := range []int{3, len(model.reqs) - 1} {
		r := model.reqs[n]
		var all strings.Builder
		for _, m := range r.Messages {
			all.WriteString(m.Content)
			all.WriteString("\n")
		}
		s := all.String()
		if !strings.Contains(s, checkpointHeader) {
			t.Fatalf("request %d has no checkpoint summary", n+1)
		}
		if !strings.Contains(s, pinnedHeader) {
			t.Fatalf("request %d has no %q section", n+1, pinnedHeader)
		}
		for i := 1; i <= 9; i++ {
			if !strings.Contains(s, fmt.Sprintf("unique-marker-%d", i)) {
				t.Errorf("request %d lost f%d.md's content", n+1, i)
			}
		}
	}
}
