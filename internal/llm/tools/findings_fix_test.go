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

// Review #3: the citation is checked against the tree the loop read; a
// record_finding naming another repository is refused, not silently
// excerpted from a different tree.
func TestRecordFindingRefusesAnotherRepository(t *testing.T) {
	ex := findingsExecutor(t)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetWorktreeRepo("enmime")
	ctx := context.Background()
	read := ex.Execute(ctx, adapter.ToolCall{ID: "r", Name: "codex_read_file", Arguments: `{"path":"a.go","start_line":1,"end_line":5}`})
	if read.IsError {
		t.Fatal(read.Content)
	}
	res := ex.Execute(ctx, adapter.ToolCall{ID: "f", Name: "record_finding", Arguments: `{"claim":"x","path":"a.go","start_line":2,"end_line":3,"repo":"other"}`})
	if !res.IsError || !strings.Contains(res.Content, "other") {
		t.Fatalf("finding on another repo = %+v, want a refusal naming the repo", res)
	}
	res = ex.Execute(ctx, adapter.ToolCall{ID: "f2", Name: "record_finding", Arguments: `{"claim":"x","path":"a.go","start_line":2,"end_line":3,"repo":"enmime"}`})
	if res.IsError {
		t.Fatalf("finding on the worktree's own repo refused: %s", res.Content)
	}
}

// Review #4: lines are counted as codex_read_file counts them, trailing
// blank lines included, so a finding on the last line shown is accepted.
func TestRecordFindingCountsTrailingBlankLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "t.go"), []byte("a\nb\n\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(dir)
	ex.markRead("t.go", 1, 4, 4)
	res := ex.Execute(context.Background(), recordCall("ends blank", "t.go", 4, 4))
	if res.IsError {
		t.Fatalf("finding on line 4 of a 4-line file refused: %s", res.Content)
	}
}

// Review #2 (spec §4): a finding makes productive only the reads it cites —
// same path, overlapping lines — not every read before it.
func TestFindingResetsOnlyTheReadItCites(t *testing.T) {
	allowed := map[string]bool{"codex_read_file": true}
	res := func() []adapter.ToolResult {
		return []adapter.ToolResult{{Content: `{"path":"a.go","start_line":1,"end_line":5,"content":"a\nb\nc\nd\ne"}`}}
	}
	r := newReadRepeats()
	for turn := 1; turn <= readRepeatLimit; turn++ {
		prior, _ := r.check(readTurn(turn), allowed)
		r.annotate(readTurn(turn), prior, res(), turn, false)
	}
	r.productive(readRepeatLimit+1, []CitedFinding{{Path: "b.go", StartLine: 1, EndLine: 5}})
	if _, stuck := r.check(readTurn(readRepeatLimit+2), allowed); stuck == nil {
		t.Fatal("a finding on another file must not reset a.go's repeats")
	}
	r.productive(readRepeatLimit+3, []CitedFinding{{Path: "a.go", StartLine: 9, EndLine: 9}})
	if _, stuck := r.check(readTurn(readRepeatLimit+4), allowed); stuck == nil {
		t.Fatal("a finding on other lines of a.go must not reset the 1-5 read")
	}
	r.productive(readRepeatLimit+5, []CitedFinding{{Path: "a.go", StartLine: 4, EndLine: 6}})
	if _, stuck := r.check(readTurn(readRepeatLimit+6), allowed); stuck != nil {
		t.Fatalf("a finding citing lines 4-6 of a.go makes the 1-5 read productive, got %v", stuck)
	}
}

// gateAdapter reads a.go, b.go, c.go without recording, then records, then
// reads d.go. results holds, per request, the previous turn's tool result
// as the model saw it.
type gateAdapter struct {
	n       int
	results []string
}

func (a *gateAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	last := ""
	for _, m := range req.Messages {
		if m.Role == adapter.RoleTool {
			last = m.Content
		}
	}
	a.results = append(a.results, last)
	read := func(p string) *adapter.CompletionResponse {
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: fmt.Sprintf("c%d", a.n), Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q,"start_line":1,"end_line":2}`, p)}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}
	}
	switch a.n {
	case 1:
		return read("a.go"), nil
	case 2:
		return read("b.go"), nil
	case 3:
		return read("c.go"), nil
	case 4:
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "f4", Name: "record_finding", Arguments: `{"claim":"b starts with b","path":"b.go","start_line":1,"end_line":1}`}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 5}, nil
	case 5:
		return read("d.go"), nil
	}
	return &adapter.CompletionResponse{Content: "done", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
}
func (a *gateAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *gateAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *gateAdapter) HealthCheck(context.Context) error { return nil }
func (a *gateAdapter) Name() string                      { return "gate/test" }

// The live run on qwen3.6:35b made 25 reads and 0 record_finding calls:
// a strategy line is not enough. With Findings, a read that follows a read
// nothing was recorded from carries a note; a second such read is not
// served — the result says to record first; after that reads are served
// again (the repeat guard still applies).
func TestUnrecordedReadsAreNudgedThenRefused(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(p[:1]+"1\n"+p[:1]+"2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex := NewExecutor(dir)
	model := &gateAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: []adapter.Tool{ReadFile(), RecordFinding()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 20, nil, nil,
		LoopOptions{Findings: true, EvidenceLedger: true, KeepRecent: 1, ReadOnly: true, StuckControl: true})
	if err != nil || resp == nil || resp.Content != "done" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(ex.Findings()) != 1 {
		t.Fatalf("findings = %d, want the one recorded in turn 4", len(ex.Findings()))
	}
	if len(model.results) < 6 {
		t.Fatalf("model got %d requests, want 6", len(model.results))
	}
	// Request n+1 carries turn n's result: read b.go (turn 2) was served
	// with the note; read c.go (turn 3) was refused; read d.go (turn 5)
	// was served clean.
	b, c, d := model.results[2], model.results[3], model.results[5]
	if !strings.Contains(b, "b1") || !strings.Contains(b, "record_finding") {
		t.Fatalf("b.go read = %q, want the content plus the record_finding note", b)
	}
	if strings.Contains(c, "c1") || !strings.Contains(c, "record_finding") {
		t.Fatalf("c.go read = %q, want a refusal naming record_finding and no content", c)
	}
	if !strings.Contains(d, "d1") || strings.Contains(d, "record_finding") {
		t.Fatalf("d.go read = %q, want the content with no note", d)
	}
}
