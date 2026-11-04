package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// runResearchTurns is runTurns with the research tool set (read_file,
// record_finding, list_files) and research-style options.
func runResearchTurns(t *testing.T, root string, turns [][]adapter.ToolCall, opts LoopOptions) []adapter.CompletionRequest {
	t.Helper()
	var resps []adapter.CompletionResponse
	for i, calls := range turns {
		for k := range calls {
			calls[k].ID = fmt.Sprintf("call_%d", k)
		}
		resps = append(resps, adapter.CompletionResponse{Content: fmt.Sprintf("step %d", i+1), ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40})
	}
	model := &scriptAdapter{turns: resps}
	ex := NewExecutor(root)
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "TASK"},
	}, Tools: []adapter.Tool{ReadFile(), RecordFinding(), ListFiles()}}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 40, nil, nil, opts); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != len(turns)+1 {
		t.Fatalf("requests = %d", len(model.reqs))
	}
	return model.reqs
}

// A research loop keeps one turn: a read leaves the model's context after
// the next call, and what it kept is a finding. A re-read of lines it
// already recorded a finding from, unchanged since, is answered with that
// finding instead of the content; lines it recorded nothing from are
// served again, because the model no longer has them.
func TestRereadOfLinesWithAFindingIsAnsweredWithTheFinding(t *testing.T) {
	root := writeTree(t, map[string]string{"src/a.ts": numberedFile(50)})
	turns := [][]adapter.ToolCall{
		{call("read_file", `{"path":"src/a.ts","start_line":1,"end_line":30}`)},
		{call("record_finding", `{"claim":"lines 10-12 set the limit","path":"src/a.ts","start_line":10,"end_line":12}`)},
		{call("read_file", `{"path":"src/a.ts","start_line":10,"end_line":12}`)}, // recorded: the finding
		{call("read_file", `{"path":"src/a.ts","start_line":20,"end_line":25}`)}, // not recorded: the content
	}
	opts := LoopOptions{EvidenceLedger: true, KeepRecent: 1, Findings: true, DedupReads: true, StuckControl: true}
	reqs := runResearchTurns(t, root, turns, opts)

	r3 := lastResults(reqs[3].Messages)
	if len(r3) != 1 || !isRereadResult(r3[0]) || !strings.Contains(r3[0], "finding #1") || !strings.Contains(r3[0], "lines 10-12 set the limit") {
		t.Fatalf("re-read of recorded lines: %v", r3)
	}
	if strings.Contains(r3[0], "line 11\n") {
		t.Fatalf("re-read of recorded lines returned the content: %q", r3[0])
	}
	r4 := lastResults(reqs[4].Messages)
	if len(r4) != 1 || !strings.HasPrefix(r4[0], "line 20\n") || !strings.Contains(r4[0], "line 25") {
		t.Fatalf("read of unrecorded lines: %v", r4)
	}
}
