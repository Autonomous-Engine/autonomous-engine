package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// findingAdapter reads a.go, records a finding on it, then answers.
type findingAdapter struct{ n int }

func (a *findingAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	switch a.n {
	case 1:
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "r1", Name: "read_file", Arguments: `{"path":"a.go","start_line":1,"end_line":3}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	case 2:
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "f1", Name: "record_finding", Arguments: `{"claim":"a.go declares package a","path":"a.go","start_line":1,"end_line":1}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *findingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *findingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *findingAdapter) HealthCheck(context.Context) error { return nil }
func (a *findingAdapter) Name() string                      { return "finding/test" }

// A loop with a database and a work item stores every recorded finding
// for the work item, with the task that recorded it.
func TestIntegration_RecordFindingIsStoredForTheWorkItem(t *testing.T) {
	pool := toolsIntegrationPool(t)
	ctx := context.Background()
	workID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title) VALUES ($1, 'feature', 'research', 'findings store')`, workID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	ex.SetFindingTask("t1", "code_trace")
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}},
		Tools:    []adapter.Tool{ReadFile(), RecordFinding()},
	}
	resp, err := RunToolLoopWithOptions(ctx, &findingAdapter{}, req, ex, 10, pool, &workID,
		LoopOptions{Findings: true, EvidenceLedger: true, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("loop: resp=%+v err=%v", resp, err)
	}
	var taskID, claim, path, excerpt string
	var start, end int
	err = pool.QueryRow(ctx, `SELECT task_id, claim, path, start_line, end_line, excerpt FROM research_findings WHERE work_item_id = $1`, workID).
		Scan(&taskID, &claim, &path, &start, &end, &excerpt)
	if err != nil {
		t.Fatalf("stored finding: %v", err)
	}
	if taskID != "t1" || claim != "a.go declares package a" || path != "a.go" || start != 1 || end != 1 || excerpt != "package a" {
		t.Fatalf("row = %s %q %s %d-%d %q", taskID, claim, path, start, end, excerpt)
	}
	// The work item owns its findings.
	if _, err := pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM research_findings WHERE work_item_id = $1`, workID).Scan(&n)
	if n != 0 {
		t.Fatalf("findings survived their work item: %d", n)
	}
}
