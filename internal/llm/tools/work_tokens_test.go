package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// bigContextAdapter reports a 12k-token prompt on every call (the whole
// context re-sent), edits a file each turn, and answers on the sixth call.
type bigContextAdapter struct{ n int }

func (a *bigContextAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	if a.n >= 6 {
		return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 12000, OutputTokens: 50}, nil
	}
	return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("e%d", a.n), Name: "append_to_file", Arguments: fmt.Sprintf(`{"path":"a.go","text":"// line %d\n"}`, a.n)}}, FinishReason: "tool_calls", InputTokens: 12000, OutputTokens: 60}, nil
}
func (a *bigContextAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *bigContextAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *bigContextAdapter) HealthCheck(context.Context) error { return nil }
func (a *bigContextAdapter) Name() string                      { return "bigctx/test" }

// The budget counted the whole prompt on every call: on a 70-call item,
// 470k of the 500k was the same context being resent, against 31k of
// output. The budget now counts what a call adds — its output plus input
// it had not sent before — so a loop that keeps producing correct edits is
// not stopped for re-reading its own context.
func TestLoopBudgetCountsNewTokensNotResentContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Six calls of 12k input would be 72k under the old meter; 30k is
	// plenty for the output and tool results they actually add.
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "30000")
	ex := NewExecutor(dir)
	var loopSpend []int64
	ex.SetIterationHook(func(_ context.Context, _, _, spend int64) error { loopSpend = append(loopSpend, spend); return nil })
	ex.markRead("a.go", 1, 1, 1)
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), AppendToFile()}}
	resp, err := RunToolLoopWithOptions(context.Background(), &bigContextAdapter{}, req, ex, 10, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"a.go"}})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("resp=%+v err=%v, want the loop to finish under a 30k budget of new tokens", resp, err)
	}
	last := loopSpend[len(loopSpend)-1]
	if last <= 0 || last > 10000 {
		t.Fatalf("loop spend seen by the hook = %v, want the new tokens only (well under 10k), not the resent 12k per call", loopSpend)
	}
}

// churnAdapter makes a refused call every turn, forever — a different
// claim each time, so it is churn without being a cycle.
type churnAdapter struct{ n int }

func (a *churnAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("c%d", a.n), Name: "record_finding", Arguments: fmt.Sprintf(`{"claim":"claim %d","path":"a.go","start_line":1,"end_line":1}`, a.n)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *churnAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *churnAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *churnAdapter) HealthCheck(context.Context) error { return nil }
func (a *churnAdapter) Name() string                      { return "churn/test" }

// Churn is the thing the budget was for: a window of tool turns in which
// nothing happened — no edit applied, no finding recorded, no command run,
// no new read or search. Eight such turns end the loop as stuck
// (no_progress); a loop that edits every turn is never stopped by it.
func TestNoProgressWindowEndsTheLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir) // a.go never read: every record_finding is refused
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	model := &churnAdapter{}
	_, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 50, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 1, StuckControl: true})
	var stuck *LoopStuck
	if !errors.As(err, &stuck) || stuck.Kind != StuckKindNoProgress {
		t.Fatalf("err = %v, want LoopStuck no_progress", err)
	}
	// The window counts executed tool turns; the cycle guard answers a
	// couple of identical turns without running them.
	if model.n < noProgressWindow || model.n > noProgressWindow+2 {
		t.Fatalf("the loop made %d calls, want about the %d-turn window", model.n, noProgressWindow)
	}
	// Progress every turn: the window never fires.
	ex2 := NewExecutor(dir)
	ex2.markRead("a.go", 1, 1, 1)
	resp, err := RunToolLoopWithOptions(context.Background(), &bigContextAdapter{}, req, ex2, 10, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 1, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("editing loop: resp=%+v err=%v", resp, err)
	}
}
