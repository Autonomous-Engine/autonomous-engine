package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// passAdapter reads a.go on its first call and answers on every other;
// firstOfPass2 is the request it got when the second pass started.
type passAdapter struct {
	n            int
	firstOfPass2 *adapter.CompletionRequest
}

func (a *passAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	switch a.n {
	case 1:
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "r1", Name: "read_file", Arguments: `{"path":"a.go"}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	case 3:
		cp := req
		a.firstOfPass2 = &cp
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *passAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *passAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *passAdapter) HealthCheck(context.Context) error { return nil }
func (a *passAdapter) Name() string                      { return "pass/test" }

// The implementation agent re-enters the tool loop for a second pass (the
// no-edit nudge, the verify gate) with the original two messages plus the
// nudge: the ledger and the pinned file contents of the first pass were
// gone, so the pass re-read every file and, live, appended the same 14
// lines to options.go twice. A LoopCarry hands the compacted transcript
// and the ledger to the next pass.
func TestLoopCarryResumesTheLedgerAcrossPasses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	model := &passAdapter{}
	carry := &LoopCarry{}
	opts := LoopOptions{EvidenceLedger: true, KeepRecent: 1, PinReads: true, PinnedFiles: []string{"a.go"}, DedupReads: true, StuckControl: true, Carry: carry}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "sys"}, {Role: adapter.RoleUser, Content: "task"}}, Tools: []adapter.Tool{ReadFile(), EditFile()}}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 10, nil, nil, opts); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(carry.Messages) == 0 {
		t.Fatal("the carry holds no transcript after pass 1")
	}
	// Pass 2 starts from the carried transcript plus the nudge.
	req.Messages = append(carry.Resume(req.Messages), adapter.ChatMessage{Role: adapter.RoleUser, Content: "edit now"})
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 10, nil, nil, opts); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if model.firstOfPass2 == nil {
		t.Fatal("pass 2 made no call")
	}
	var ledgerMsg string
	for _, m := range model.firstOfPass2.Messages {
		if isLedgerMessage(m) {
			ledgerMsg = m.Content
		}
	}
	if ledgerMsg == "" || !strings.Contains(ledgerMsg, "a.go") || !strings.Contains(ledgerMsg, pinnedHeader) || !strings.Contains(ledgerMsg, "func A() {}") {
		t.Fatalf("pass 2's first request lacks the carried ledger with a.go pinned:\n%s", ledgerMsg)
	}
	if last := model.firstOfPass2.Messages[len(model.firstOfPass2.Messages)-1]; last.Content != "edit now" {
		t.Fatalf("the nudge must be the last message, got %q", last.Content)
	}
	if carry.Resume(nil) == nil {
		t.Fatal("Resume returns the carried transcript")
	}
	if (&LoopCarry{}).Resume(req.Messages) == nil {
		t.Fatal("an empty carry resumes from the fallback")
	}
}
