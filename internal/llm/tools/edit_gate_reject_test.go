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

// catch22Adapter reads a.go, b.go, c.go (the gate's three turns), then
// appends to d.go without having read it (rejected by the read-before-edit
// gate), then reads d.go (which the edit gate must serve: the rejection
// asked for it), then appends again, then answers.
type catch22Adapter struct {
	n       int
	results []string
}

func (a *catch22Adapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	last := ""
	for _, m := range req.Messages {
		if m.Role == adapter.RoleTool {
			last = m.Content
		}
	}
	a.results = append(a.results, last)
	call := func(name, args string) *adapter.CompletionResponse {
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("c%d", a.n), Name: name, Arguments: args}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}
	}
	switch a.n {
	case 1, 2, 3:
		return call("read_file", fmt.Sprintf(`{"path":"%c.go"}`, 'a'+a.n-1)), nil
	case 4, 6:
		return call("append_to_file", `{"path":"d.go","text":"// appended\n"}`), nil
	case 5:
		return call("read_file", `{"path":"d.go"}`), nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *catch22Adapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *catch22Adapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *catch22Adapter) HealthCheck(context.Context) error { return nil }
func (a *catch22Adapter) Name() string                      { return "catch22/test" }

// Live (ciblame #8): after four applied edits the model appended the test,
// the read-before-edit gate rejected it ("read the file first"), and the
// edit gate refused that very read — the attempt burned its last 60k
// tokens between the two gates. A read of the path a rejection named is
// always served.
func TestEditGateServesTheReadARejectionAskedFor(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(dir, p+".go"), []byte("package "+p+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := NewExecutor(dir)
	model := &catch22Adapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), EditFile(), AppendToFile()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil,
		LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"d.go"}, DedupReads: true, StuckControl: true, EditGate: 3})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(model.results) < 7 {
		t.Fatalf("model got %d requests, want 7", len(model.results))
	}
	if r := model.results[4]; !strings.HasPrefix(r, "REJECTED") {
		t.Fatalf("turn 4 (append before reading) = %q, want the read-before-edit rejection", r)
	}
	if r := model.results[5]; !strings.Contains(r, "package d") || strings.Contains(r, "not served") {
		t.Fatalf("turn 5 (the read the rejection asked for) = %q, want it served", r)
	}
	if r := model.results[6]; strings.HasPrefix(r, "REJECTED") || !strings.Contains(strings.ToLower(r), "append") && !strings.Contains(strings.ToLower(r), "inserted") {
		t.Fatalf("turn 6 (append after reading) = %q, want it applied", r)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "d.go"))
	if !strings.Contains(string(data), "// appended") {
		t.Fatalf("d.go not appended: %q", data)
	}
}
