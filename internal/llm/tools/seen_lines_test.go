package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// Live, code_trace ended "stuck": record_finding refused lines the model
// had seen in codex_grep hits ("you have not read inspect.go lines 28-86")
// three times. A line a search showed counts as seen for a citation of
// that line; lines around it that were never shown still do not.
func TestGrepHitsCountAsSeenForFindings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n// A does a thing.\nfunc A() {}\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	ex.SetCodexReadsFromWorktree(true)
	ctx := context.Background()
	res := ex.Execute(ctx, adapter.ToolCall{ID: "g", Name: "codex_grep", Arguments: `{"pattern":"func A"}`})
	if res.IsError || !strings.Contains(res.Content, `"line": 4`) {
		t.Fatalf("grep = %+v", res)
	}
	if r := ex.Execute(ctx, recordCall("A is declared", "a.go", 4, 4)); r.IsError {
		t.Fatalf("finding on the grep hit refused: %s", r.Content)
	}
	if r := ex.Execute(ctx, recordCall("the whole file", "a.go", 1, 6)); !r.IsError || !strings.Contains(r.Content, "read") {
		t.Fatalf("finding on unseen lines = %+v, want a refusal", r)
	}
}

// refusingAdapter records three findings on lines it never read, then
// answers.
type refusingAdapter struct{ n int }

func (a *refusingAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	if a.n <= 3 {
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("f%d", a.n), Name: "record_finding", Arguments: `{"claim":"x","path":"a.go","start_line":1,"end_line":2}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *refusingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *refusingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *refusingAdapter) HealthCheck(context.Context) error { return nil }
func (a *refusingAdapter) Name() string                      { return "refusing/test" }

// A refused record_finding is guidance, not a failing file edit: three of
// them do not end the loop as stuck.
func TestRecordFindingRefusalsDoNotEndTheLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	resp, err := RunToolLoopWithOptions(context.Background(), &refusingAdapter{}, req, ex, 10, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("resp=%+v err=%v, want the answer", resp, err)
	}
}

// A task that reaches its cap with tool calls run but nothing recorded
// still closes with an answer (live: impact_assessment hit its slice after
// 9 iterations with no finding and the round failed on it).
func TestLoopClosesWithAnAnswerAfterToolCallsEvenWithoutFindings(t *testing.T) {
	ex := NewExecutor(closingTree(t))
	model := &gateAdapter{} // reads a.go, b.go, c.go, records once (turn 4), reads d.go, answers
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 3, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.FinishReason != "closed_at_limit" {
		t.Fatalf("resp=%+v err=%v, want a closing answer after 3 read turns with no finding", resp, err)
	}
}
