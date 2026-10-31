package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func ledgerMessage(msgs []adapter.ChatMessage) string {
	for _, m := range msgs {
		if isLedgerMessage(m) {
			return m.Content
		}
	}
	return ""
}

// T4: the implementation ledger keeps the latest read of an edited file
// (or a file in scope) in full, refreshed from disk, until a newer read
// replaces it; old test logs become one ledger line.
func TestLedgerPinsReadsOfEditedAndScopedFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/server.ts": "import express from 'express';\nexport const app = express();\napp.get('/health', h);\n",
		"src/env.ts":    "export const config = {\n  port: 8080,\n};\n",
		"README.md":     "# sample-app\n",
	})
	noisyLog := strings.Repeat("{\"level\":30,\"msg\":\"Loaded index\"}\n", 150) +
		" × Bookshelf API > GET /api/settings > returns the key\n Test Files  1 failed | 27 passed (28)\n\n[exit code: exit status 1]"
	calls := []adapter.ToolCall{
		{Name: "read_file", Arguments: `{"path":"src/server.ts"}`},
		{Name: "read_file", Arguments: `{"path":"src/env.ts"}`},
		{Name: "edit_file", Arguments: `{"path":"src/server.ts","old_string":"app.get('/health', h);","new_string":"app.get('/health', h);\napp.get('/api/settings', cfg);"}`},
		{Name: "run_command", Arguments: `{"command":"npm test"}`},
		{Name: "read_file", Arguments: `{"path":"README.md"}`},
		{Name: "list_files", Arguments: `{"path":"src"}`},
		{Name: "list_files", Arguments: `{"path":"."}`},
		{Name: "read_file", Arguments: `{"path":"src/server.ts"}`},
		{Name: "list_files", Arguments: `{"path":"tests"}`},
		{Name: "list_files", Arguments: `{"path":"docs"}`},
		{Name: "list_files", Arguments: `{"path":"public"}`},
	}
	var turns []adapter.CompletionResponse
	for i, c := range calls {
		c.ID = "call_0" // providers restart ids every turn
		turns = append(turns, adapter.CompletionResponse{Content: fmt.Sprintf("step %d", i+1), ToolCalls: []adapter.ToolCall{c}, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40})
	}
	model := &scriptAdapter{turns: turns}
	ex := NewExecutor(root)
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		if call.Name == "run_command" {
			return adapter.ToolResult{Content: noisyLog}, true
		}
		return adapter.ToolResult{}, false // the real read/edit/list tools
	})
	task := "TASK: add /api/settings"
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: task},
	}, Tools: ImplementationToolsFor(ImplementationPhaseEdit, ImplementationToolOptions{})}
	opts := LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"./src/env.ts"}}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 40, nil, nil, opts); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != len(calls)+1 {
		t.Fatalf("requests = %d", len(model.reqs))
	}
	for i, r := range model.reqs {
		if r.Messages[1].Content != task {
			t.Fatalf("request %d: the task is not the first user message", i+1)
		}
		assertProtocol(t, r.Messages)
	}

	// Request 5 (after 4 iterations): the server.ts read (iteration 1) has
	// left the transcript, but server.ts was edited: its CURRENT content is
	// pinned. env.ts is in scope: pinned. The noisy test log is still recent.
	l5 := ledgerMessage(model.reqs[4].Messages)
	for _, want := range []string{"## Current file contents", "### src/server.ts", "app.get('/api/settings', cfg);", "### src/env.ts", "port: 8080"} {
		if !strings.Contains(l5, want) {
			t.Errorf("request 5 ledger lacks %q:\n%s", want, l5)
		}
	}
	// Request 7: the npm test result is a single ledger line now.
	l7 := ledgerMessage(model.reqs[6].Messages)
	if !strings.Contains(l7, `run_command("command":"npm test") -> exit 1: × Bookshelf API`) {
		t.Errorf("request 7 ledger has no one-line npm test entry:\n%s", l7)
	}
	if strings.Contains(l7, "Loaded index") {
		t.Error("request 7 still carries the old test log")
	}
	// README.md was neither edited nor in scope: ledgered, not pinned.
	if strings.Contains(l7, "### README.md") {
		t.Error("README.md (not edited, not in scope) is pinned")
	}
	// Request 9: server.ts was read again at iteration 8 (still in the
	// transcript): the pinned copy gives way to it; env.ts stays.
	l9 := ledgerMessage(model.reqs[8].Messages)
	if strings.Contains(l9, "### src/server.ts") || !strings.Contains(l9, "### src/env.ts") {
		t.Errorf("request 9 pins:\n%s", l9)
	}
	// Request 12: the second server.ts read left too: pinned again.
	l12 := ledgerMessage(model.reqs[11].Messages)
	if !strings.Contains(l12, "### src/server.ts") || strings.Count(l12, "### src/server.ts") != 1 {
		t.Errorf("request 12 pins:\n%s", l12)
	}
}

// Without PinReads (research) nothing is pinned.
func TestLedgerDoesNotPinWithoutTheOption(t *testing.T) {
	root := writeTree(t, map[string]string{"a.ts": "export const a = 1;\n"})
	var turns []adapter.CompletionResponse
	for i, c := range []adapter.ToolCall{
		{Name: "read_file", Arguments: `{"path":"a.ts"}`},
		{Name: "list_files", Arguments: `{"path":"."}`},
		{Name: "list_files", Arguments: `{"path":"x"}`},
		{Name: "list_files", Arguments: `{"path":"y"}`},
	} {
		c.ID = fmt.Sprintf("c%d", i)
		turns = append(turns, adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{c}, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40})
	}
	model := &scriptAdapter{turns: turns}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "t"}},
		Tools: ImplementationToolsFor(ImplementationPhaseEdit, ImplementationToolOptions{})}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(root), 40, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinnedFiles: []string{"a.ts"}}); err != nil {
		t.Fatal(err)
	}
	if l := ledgerMessage(model.reqs[len(model.reqs)-1].Messages); strings.Contains(l, "Current file contents") {
		t.Errorf("pinned without PinReads:\n%s", l)
	}
}
