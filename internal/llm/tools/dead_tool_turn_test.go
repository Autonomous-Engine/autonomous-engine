package tools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// B5: live-run attempt 6 made 15 codex_workspace_symbols calls in one turn
// and all 15 failed "workspace not ready" — the guard only counted after
// the turn. With DeadToolGuard on, the tool runs at most twice in a turn,
// the rest are answered without running it, and the next request no longer
// offers it. Other calls in the same turn still run.
func TestDeadToolGuardShortCircuitsWithinOneTurn(t *testing.T) {
	var calls []adapter.ToolCall
	for i := 0; i < 15; i++ {
		calls = append(calls, adapter.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "codex_workspace_symbols", Arguments: fmt.Sprintf(`{"query":"Sym%d"}`, i)})
	}
	calls = append(calls, adapter.ToolCall{ID: "call_15", Name: "codex_grep", Arguments: `{"pattern":"x"}`})
	model := &scriptAdapter{turns: []adapter.CompletionResponse{
		{ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 300},
	}}
	var lspRuns, grepRuns atomic.Int32
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		if call.Name == "codex_workspace_symbols" {
			lspRuns.Add(1)
			return adapter.ToolResult{Content: "codex_workspace_symbols: lsp: typescript workspace not ready: context deadline exceeded", IsError: true}, true
		}
		grepRuns.Add(1)
		return adapter.ToolResult{Content: `{"total_hits":0,"hits":[]}`}, true
	})
	req := baseRequest()
	req.Tools = append(ResearchToolsFor("code_trace", ResearchToolOptions{LSPReady: true}), adapter.Tool{Name: "codex_grep"})
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 10, nil, nil, LoopOptions{DeadToolGuard: true}); err != nil {
		t.Fatal(err)
	}
	if n := lspRuns.Load(); n > 2 {
		t.Fatalf("the not-ready tool ran %d times in one turn, want at most 2", n)
	}
	if grepRuns.Load() != 1 {
		t.Fatalf("codex_grep ran %d times, want 1", grepRuns.Load())
	}
	if len(model.reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(model.reqs))
	}
	next := model.reqs[1]
	for _, tl := range next.Tools {
		if tl.Name == "codex_workspace_symbols" {
			t.Fatal("the next request still offers the dead tool")
		}
	}
	assertProtocol(t, next.Messages)
	skipped := 0
	for _, m := range next.Messages {
		if m.Role == adapter.RoleTool && strings.Contains(m.Content, "not run") {
			skipped++
		}
	}
	if skipped != 13 {
		t.Fatalf("%d calls answered without running, want 13", skipped)
	}
}

// One success among a tool's calls in a turn ends the probing: the rest run.
func TestDeadToolGuardRunsTheRestAfterASuccess(t *testing.T) {
	var calls []adapter.ToolCall
	for i := 0; i < 5; i++ {
		calls = append(calls, adapter.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "codex_workspace_symbols", Arguments: fmt.Sprintf(`{"query":"Sym%d"}`, i)})
	}
	model := &scriptAdapter{turns: []adapter.CompletionResponse{{ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 100}}}
	var runs atomic.Int32
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		runs.Add(1)
		return adapter.ToolResult{Content: `{"symbols":[]}`}, true
	})
	req := baseRequest()
	req.Tools = ResearchToolsFor("code_trace", ResearchToolOptions{LSPReady: true})
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 10, nil, nil, LoopOptions{DeadToolGuard: true}); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 5 {
		t.Fatalf("ran %d of 5 calls", runs.Load())
	}
}
