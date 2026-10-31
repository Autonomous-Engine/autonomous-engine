package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// lastResults are the tool results of the latest tool-call turn of a
// request, in call order.
func lastResults(msgs []adapter.ChatMessage) []string {
	at := -1
	for i, m := range msgs {
		if m.Role == adapter.RoleAssistant && len(m.ToolCalls) > 0 {
			at = i
		}
	}
	var out []string
	for i := at + 1; at >= 0 && i < len(msgs); i++ {
		if msgs[i].Role == adapter.RoleTool {
			out = append(out, msgs[i].Content)
		}
	}
	return out
}

// runTurns runs the loop over turns (one slice of calls each) and returns
// the model's requests.
func runTurns(t *testing.T, root string, turns [][]adapter.ToolCall, opts LoopOptions) []adapter.CompletionRequest {
	t.Helper()
	var resps []adapter.CompletionResponse
	for i, calls := range turns {
		for k := range calls {
			calls[k].ID = fmt.Sprintf("call_%d", k) // providers restart ids every turn
		}
		resps = append(resps, adapter.CompletionResponse{Content: fmt.Sprintf("step %d", i+1), ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40})
	}
	model := &scriptAdapter{turns: resps}
	ex := NewExecutor(root)
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "TASK"},
	}, Tools: ImplementationToolsFor(ImplementationPhaseEdit, ImplementationToolOptions{})}
	if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 40, nil, nil, opts); err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != len(turns)+1 {
		t.Fatalf("requests = %d", len(model.reqs))
	}
	return model.reqs
}

func call(name, args string) adapter.ToolCall { return adapter.ToolCall{Name: name, Arguments: args} }

// R12 X2: a read of lines the model still has (a recent result, or a
// pinned read), unchanged since, is answered with one line instead of the
// content; after an edit the next read returns the content.
func TestRereadOfUnchangedLinesIsShort(t *testing.T) {
	root := writeTree(t, map[string]string{"src/a.ts": numberedFile(50), "src/b.ts": numberedFile(10)})
	readA := call("read_file", `{"path":"src/a.ts"}`)
	turns := [][]adapter.ToolCall{
		{readA}, // 1: the content
		// 2: the same read twice and a range inside 1-50: unchanged (turn
		// 1's result is still in the transcript the model sees next).
		{readA, readA, call("read_file", `{"path":"src/a.ts","start_line":10,"end_line":20}`)},
		{call("edit_file", `{"path":"src/a.ts","old_string":"line 5\n","new_string":"line 5 edited\n"}`)},
		{readA}, // 4: changed: the content again
		{call("list_files", `{"path":"src"}`)},
		{call("list_files", `{"path":"."}`)},
		{call("list_files", `{"path":"src/x"}`)},
	}
	// KeepRecent 3: turn 1's result is still in the transcript at turn 4,
	// so turn 4's read is checked against it (and not against a pin, which
	// would show the edit and rightly answer "unchanged").
	reqs := runTurns(t, root, turns, LoopOptions{EvidenceLedger: true, KeepRecent: 3, PinReads: true, DedupReads: true})

	if r := lastResults(reqs[1].Messages); len(r) != 1 || !strings.HasPrefix(r[0], "line 1\n") {
		t.Fatalf("turn 1: %v", r)
	}
	r2 := lastResults(reqs[2].Messages)
	if len(r2) != 3 {
		t.Fatalf("turn 2: %v", r2)
	}
	for i, r := range r2 {
		if r != "src/a.ts: unchanged since your last read of lines 1-50; edit directly or request a different range" {
			t.Fatalf("turn 2 read %d: %q", i+1, r)
		}
	}
	if r := lastResults(reqs[4].Messages); len(r) != 1 || !strings.Contains(r[0], "line 5 edited\n") {
		t.Fatalf("turn 4 (after the edit): %v", r)
	}
	// Turn 4 returns the whole file.
	if !strings.Contains(strings.Join(lastResults(reqs[4].Messages), ""), "line 50") {
		t.Fatal("turn 4 is not the whole file")
	}
	// The ledger says the re-reads were re-reads.
	l := ledgerMessage(reqs[len(reqs)-1].Messages)
	if !strings.Contains(l, `- read_file("path":"src/a.ts") -> re-read, unchanged`) {
		t.Fatalf("ledger:\n%s", l)
	}
}

// A read whose earlier result has left the transcript but is pinned (in
// scope) is answered "(pinned)"; a read of lines outside the pin, or of a
// file never read, gets the content.
func TestRereadOfAPinnedReadIsShort(t *testing.T) {
	root := writeTree(t, map[string]string{"src/a.ts": numberedFile(50), "src/b.ts": numberedFile(10)})
	turns := [][]adapter.ToolCall{
		{call("read_file", `{"path":"src/a.ts","start_line":1,"end_line":30}`)},
		{call("list_files", `{"path":"src"}`)},
		{call("list_files", `{"path":"."}`)},
		{call("read_file", `{"path":"src/a.ts","start_line":5,"end_line":25}`)},  // pinned 1-30
		{call("read_file", `{"path":"src/a.ts","start_line":25,"end_line":40}`)}, // past the pin
		{call("read_file", `{"path":"src/b.ts"}`)},
	}
	reqs := runTurns(t, root, turns, LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true, PinnedFiles: []string{"src/a.ts"}, DedupReads: true})
	if !strings.Contains(ledgerMessage(reqs[3].Messages), "### src/a.ts (lines 1-30)") {
		t.Fatalf("not pinned:\n%s", ledgerMessage(reqs[3].Messages))
	}
	if r := lastResults(reqs[4].Messages); len(r) != 1 || r[0] != "src/a.ts: unchanged since your last read of lines 1-30 (pinned); edit directly or request a different range" {
		t.Fatalf("pinned re-read: %v", r)
	}
	// The pin stays: a short answer is not a newer read of the file.
	if !strings.Contains(ledgerMessage(reqs[4].Messages), "### src/a.ts (lines 1-30)") {
		t.Fatalf("pin dropped after the re-read:\n%s", ledgerMessage(reqs[4].Messages))
	}
	if r := lastResults(reqs[5].Messages); len(r) != 1 || !strings.HasPrefix(r[0], "line 25\n") {
		t.Fatalf("read past the pin: %v", r)
	}
	if r := lastResults(reqs[6].Messages); len(r) != 1 || !strings.HasPrefix(r[0], "line 1\n") {
		t.Fatalf("first read of b.ts: %v", r)
	}
}

// Without DedupReads (research loops) every read returns its content.
func TestRereadDedupIsOptIn(t *testing.T) {
	root := writeTree(t, map[string]string{"src/a.ts": numberedFile(50)})
	readA := call("read_file", `{"path":"src/a.ts"}`)
	reqs := runTurns(t, root, [][]adapter.ToolCall{{readA}, {readA}}, LoopOptions{EvidenceLedger: true, KeepRecent: 2, PinReads: true})
	if r := lastResults(reqs[2].Messages); len(r) != 1 || !strings.HasPrefix(r[0], "line 1\n") {
		t.Fatalf("second read: %v", r)
	}
}
