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

// exploringAdapter reads a new range and records a finding every turn,
// forever; with no tools offered it writes its answer.
type exploringAdapter struct {
	n        int
	closing  []string // user messages seen when no tools were offered
	toolless int
}

func (a *exploringAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	if len(req.Tools) == 0 {
		a.toolless++
		last := req.Messages[len(req.Messages)-1]
		a.closing = append(a.closing, last.Content)
		return &adapter.CompletionResponse{Content: "answer from findings", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
	}
	if a.n%2 == 1 {
		line := (a.n + 1) / 2
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("r%d", a.n), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"a.go","start_line":%d,"end_line":%d}`, line, line)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	}
	line := a.n / 2
	return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("f%d", a.n), Name: "record_finding", Arguments: fmt.Sprintf(`{"claim":"line %d says so","path":"a.go","start_line":%d,"end_line":%d}`, line, line, line)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *exploringAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *exploringAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *exploringAdapter) HealthCheck(context.Context) error { return nil }
func (a *exploringAdapter) Name() string                      { return "exploring/test" }

func closingTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A research task that reaches its iteration cap with findings recorded
// ends with an answer written from them (one closing call, no tools)
// instead of a safety-limit error: live, pattern_match explored for 25
// iterations and returned nothing.
func TestLoopClosesWithAnAnswerFromFindingsAtTheIterationCap(t *testing.T) {
	ex := NewExecutor(closingTree(t))
	model := &exploringAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 6, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "answer from findings" {
		t.Fatalf("resp=%+v err=%v, want the closing answer", resp, err)
	}
	if model.toolless != 1 || len(model.closing) != 1 || !strings.Contains(model.closing[0], "findings") || !strings.Contains(strings.ToLower(model.closing[0]), "no more tool calls") {
		t.Fatalf("closing call: toolless=%d messages=%q", model.toolless, model.closing)
	}
	if resp.FinishReason != "closed_at_limit" {
		t.Fatalf("finish reason = %q, want closed_at_limit", resp.FinishReason)
	}
	if n := len(ex.Findings()); n != 3 {
		t.Fatalf("findings = %d, want 3 (6 iterations: 3 reads, 3 findings)", n)
	}
}

// The same when the caller's budget hook stops the loop (the research
// share): one closing call is allowed past the share, then the answer.
func TestLoopClosesWithAnAnswerWhenTheHookStops(t *testing.T) {
	ex := NewExecutor(closingTree(t))
	calls := 0
	ex.SetIterationHook(func(context.Context, int64, int64, int64) error {
		calls++
		if calls > 4 {
			return fmt.Errorf("%w: research stage used its share", ErrTokenBudgetExceeded)
		}
		return nil
	})
	model := &exploringAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 50, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "answer from findings" || resp.FinishReason != "closed_at_limit" {
		t.Fatalf("resp=%+v err=%v, want the closing answer", resp, err)
	}
	// Without findings there is nothing to answer from: the error stands.
	ex2 := NewExecutor(closingTree(t))
	ex2.SetIterationHook(func(context.Context, int64, int64, int64) error { return fmt.Errorf("%w: share", ErrTokenBudgetExceeded) })
	_, err = RunToolLoopWithOptions(context.Background(), &exploringAdapter{}, req, ex2, 50, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("no findings: err = %v, want the budget error", err)
	}
}

// Old assistant turns are trimmed to 300 chars each by compaction; over 25
// turns that was 2k tokens of stubs on every call. With Findings only the
// last few stubs stay — the ledger holds what the older turns did.
func TestCompactionKeepsOnlyRecentAssistantStubsWithFindings(t *testing.T) {
	var msgs []adapter.ChatMessage
	msgs = append(msgs, adapter.ChatMessage{Role: adapter.RoleUser, Content: "go"})
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			adapter.ChatMessage{Role: adapter.RoleAssistant, Content: strings.Repeat("thinking ", 50), ToolCalls: []adapter.ToolCall{{ID: id, Name: "codex_grep", Arguments: fmt.Sprintf(`{"pattern":"p%d"}`, i)}}},
			adapter.ChatMessage{Role: adapter.RoleTool, ToolCallID: id, Name: "codex_grep", Content: "hit"})
	}
	ledger := newEvidenceLedger()
	ledger.findings = func() []CitedFinding { return nil }
	out := compactTranscript(msgs, ledger, 1)
	stubs := 0
	for _, m := range out {
		if m.Role == adapter.RoleAssistant && len(m.ToolCalls) == 0 {
			stubs++
		}
	}
	if stubs > ledgerAssistantStubsWithFindings {
		t.Fatalf("compacted transcript keeps %d assistant stubs, want at most %d", stubs, ledgerAssistantStubsWithFindings)
	}
}
