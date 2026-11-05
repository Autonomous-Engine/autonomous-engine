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

// browsingAdapter reads a.go … e.go one per turn, then edits a.go, then
// reads f.go, then answers; results holds the previous turn's tool result
// as the model saw it, per request.
type browsingAdapter struct {
	n       int
	results []string
}

func (a *browsingAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	last := ""
	for _, m := range req.Messages {
		if m.Role == adapter.RoleTool {
			last = m.Content
		}
	}
	a.results = append(a.results, last)
	read := func(p string) *adapter.CompletionResponse {
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("c%d", a.n), Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, p)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}
	}
	switch a.n {
	case 1, 2, 3, 4, 5:
		return read(fmt.Sprintf("%c.go", 'a'+a.n-1)), nil
	case 6:
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "e6", Name: "edit_file", Arguments: `{"path":"a.go","old_string":"a1","new_string":"a1 edited"}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	case 7:
		return read("f.go"), nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *browsingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *browsingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *browsingAdapter) HealthCheck(context.Context) error { return nil }
func (a *browsingAdapter) Name() string                      { return "browsing/test" }

// Live, the implementation agent spent 21 calls (243k tokens) reading and
// narrating without one edit, through three "you did not call edit_file"
// nudges. With EditGate, after that many read turns without an edit the
// next reads are not served — the files in scope are pinned — until an
// edit is made; then reads are served again.
func TestEditGateRefusesReadsAfterUneditedReadTurns(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := os.WriteFile(filepath.Join(dir, p+".go"), []byte(p+"1\n"+p+"2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := NewExecutor(dir)
	model := &browsingAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), EditFile()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil,
		LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"a.go"}, DedupReads: true, StuckControl: true, EditGate: 3})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(model.results) < 8 {
		t.Fatalf("model got %d requests, want 8", len(model.results))
	}
	// Request n+1 carries turn n's result. Reads 1-3 served; reads 4 and 5
	// refused with the edit instruction; the edit applied; read 7 served.
	for turn, want := range map[int]string{1: "a1", 2: "b1", 3: "c1", 7: "f1"} {
		if got := model.results[turn]; !strings.Contains(got, want) || strings.Contains(got, "edit_file") {
			t.Errorf("turn %d result = %q, want the file content served", turn, got)
		}
	}
	for _, turn := range []int{4, 5} {
		if got := model.results[turn]; strings.Contains(got, "d1") || strings.Contains(got, "e1") || !strings.Contains(got, "edit_file") {
			t.Errorf("turn %d result = %q, want a refusal naming edit_file and no content", turn, got)
		}
	}
	if got := model.results[6]; !strings.Contains(got, "Edit applied") {
		t.Errorf("turn 6 result = %q, want the edit applied", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if !strings.Contains(string(data), "a1 edited") {
		t.Fatalf("a.go not edited: %q", data)
	}
}

// scopeAdapter reads b.go, c.go, d.go (the gate's three turns), then a.go
// (in scope), then e.go, then answers.
type scopeAdapter struct {
	n       int
	results []string
}

func (a *scopeAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	last := ""
	for _, m := range req.Messages {
		if m.Role == adapter.RoleTool {
			last = m.Content
		}
	}
	a.results = append(a.results, last)
	read := func(p string) *adapter.CompletionResponse {
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("c%d", a.n), Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q,"start_line":1,"end_line":1}`, p)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}
	}
	switch a.n {
	case 1:
		return read("b.go"), nil
	case 2:
		return read("c.go"), nil
	case 3:
		return read("d.go"), nil
	case 4:
		return read("a.go"), nil
	case 5:
		return read("e.go"), nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *scopeAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *scopeAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *scopeAdapter) HealthCheck(context.Context) error { return nil }
func (a *scopeAdapter) Name() string                      { return "scope/test" }

// The files in scope are what the task edits: the gate never refuses a
// read of one of them (live, ciblame: report.go refused twelve times for
// ~100k tokens while it was the one file to change). It limits reads
// elsewhere.
func TestEditGateNeverRefusesInScopeFiles(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(dir, p+".go"), []byte(p+"1\n"+p+"2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := NewExecutor(dir)
	model := &scopeAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), EditFile()}}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil,
		LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"a.go"}, DedupReads: true, StuckControl: true, EditGate: 3}); err != nil {
		t.Fatal(err)
	}
	if got := model.results[4]; !strings.Contains(got, "a1") || strings.Contains(got, "not served") {
		t.Fatalf("read of the in-scope file after the gate = %q, want it served", got)
	}
	if got := model.results[5]; strings.Contains(got, "e1") || !strings.Contains(got, "not served") {
		t.Fatalf("read of an out-of-scope file after the gate = %q, want it refused", got)
	}
}
