package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// Live (enmime, 2026-11-03 15:54), a research model wrote 24k characters
// of prose around one tool call; the loop kept that assistant turn whole
// inside the one-turn window, so the next call carried it all again (17k
// and 19k input tokens, 6k of them the model's own words), and the work
// meter counted them. The tool calls are what the history needs; prose
// over assistantProseMax is kept as its head and tail with a note.
func TestLongAssistantProseIsTrimmedInTheTranscript(t *testing.T) {
	root := writeTree(t, map[string]string{"src/a.ts": numberedFile(20)})
	prose := "## Plan\nI will read the file.\n" + strings.Repeat("The parser handles each header in turn, which matters because ", 400) + "\nNEXT: read src/a.ts."
	if len(prose) < 20000 {
		t.Fatalf("fixture prose is %d chars", len(prose))
	}
	model := &scriptAdapter{turns: []adapter.CompletionResponse{
		{Content: prose, ToolCalls: []adapter.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"src/a.ts"}`}}, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 6000},
		{Content: "short", ToolCalls: []adapter.ToolCall{{ID: "c2", Name: "list_files", Arguments: `{"path":"src"}`}}, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 10},
	}}
	ex := NewExecutor(root)
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "TASK"},
	}, Tools: []adapter.Tool{ReadFile(), ListFiles()}}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 10, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 3 {
		t.Fatalf("requests = %d", len(model.reqs))
	}
	var kept *adapter.ChatMessage
	for i := range model.reqs[1].Messages {
		if m := &model.reqs[1].Messages[i]; m.Role == adapter.RoleAssistant && len(m.ToolCalls) > 0 {
			kept = m
		}
	}
	if kept == nil {
		t.Fatal("the assistant turn is not in the next request")
	}
	if len(kept.Content) > assistantProseMax+200 {
		t.Fatalf("assistant prose resent whole: %d chars", len(kept.Content))
	}
	if !strings.HasPrefix(kept.Content, "## Plan") || !strings.HasSuffix(kept.Content, "NEXT: read src/a.ts.") || !strings.Contains(kept.Content, "trimmed") {
		t.Fatalf("trimmed prose keeps neither head nor tail nor says so:\n%s", kept.Content)
	}
	if len(kept.ToolCalls) != 1 || kept.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls = %+v", kept.ToolCalls)
	}
	// Short prose is kept as it is; the final answer is the loop's result,
	// never trimmed.
	for _, m := range model.reqs[2].Messages {
		if m.Role == adapter.RoleAssistant && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "c2" && m.Content != "short" {
			t.Fatalf("short prose changed: %q", m.Content)
		}
	}
	if resp.Content != "Final answer: the flow is in a.ts:12." {
		t.Fatalf("final = %q", resp.Content)
	}
}

func TestTrimAssistantProseKeepsHeadAndTail(t *testing.T) {
	if got := trimAssistantProse("short"); got != "short" {
		t.Fatalf("short changed: %q", got)
	}
	long := strings.Repeat("a", assistantProseMax) + strings.Repeat("b", 5000) + "tail"
	got := trimAssistantProse(long)
	if len(got) > assistantProseMax+200 || !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "tail") || !strings.Contains(got, "5004 chars trimmed") {
		t.Fatalf("trimmed = %d chars, %q…%q", len(got), got[:20], got[len(got)-60:])
	}
}
