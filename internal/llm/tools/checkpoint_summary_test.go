package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// turn is one assistant turn with its tool calls and their results.
func turn(prose string, calls ...[3]string) []adapter.ChatMessage {
	a := adapter.ChatMessage{Role: adapter.RoleAssistant, Content: prose}
	var res []adapter.ChatMessage
	for i, c := range calls {
		id := "call_" + string(rune('0'+i)) // providers restart ids every turn
		a.ToolCalls = append(a.ToolCalls, adapter.ToolCall{ID: id, Name: c[0], Arguments: c[1]})
		res = append(res, adapter.ChatMessage{Role: adapter.RoleTool, ToolCallID: id, Name: c[0], Content: c[2]})
	}
	return append([]adapter.ChatMessage{a}, res...)
}

// attempt6Transcript is shaped like live-run attempt 6's implementation
// loop: codex reads, an edit, a failing npm test, a grep.
func attempt6Transcript() []adapter.ChatMessage {
	msgs := []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "TASK-MARKER: Implement in sample-app: Configure the search API key"},
	}
	msgs = append(msgs, turn("I will read the config first.",
		[3]string{"codex_read_file", `{"path":"src/config/env.ts"}`, `{"path":"src/config/env.ts","start_line":1,"end_line":40,"total_lines":40,"content":"export const config = {"}`},
		[3]string{"codex_grep", `{"pattern":"SEARCH","glob":"**/*.html"}`, `{"pattern":"SEARCH","total_hits":2,"hits":[{"file":"public/index.html","line":840,"text":"x"},{"file":"landing/index.html","line":855,"text":"y"}]}`},
	)...)
	msgs = append(msgs, turn("",
		[3]string{"edit_file", `{"path":"src/config/env.ts","old_string":"port:","new_string":"  searchApiKey: process.env.SEARCH_API_KEY || '',\n  port:"}`, "Edit applied to src/config/env.ts: replaced 1 occurrence"},
	)...)
	msgs = append(msgs, turn(`{"name":"run_command","arguments":{"command":"npm test"}}`,
		[3]string{"run_command", `{"command":"npm test"}`, " RUN  v1.6.0\n ✓ tests/unit/books.test.ts (12)\n × tests/unit/server.test.ts > settings > returns the key\n   AssertionError: expected '' to be 'test-key'\n Test Files  1 failed | 22 passed (23)\n\n[exit code: exit status 1]"},
	)...)
	msgs = append(msgs, turn("",
		[3]string{"read_file", `{"path":"src/server/index.ts","start_line":30,"end_line":90}`, "app.get('/health')"},
		[3]string{"codex_read_file", `{"path":"docs/missing.md"}`, `codex_read_file: cannot open "docs/missing.md" (repo=sample-app): no such file`},
	)...)
	// The tail the checkpoint keeps.
	msgs = append(msgs, turn("TAIL-MARKER",
		[3]string{"run_command", `{"command":"npm run lint"}`, "TAIL-RESULT ok"},
	)...)
	msgs = append(msgs, turn("TAIL-MARKER 2",
		[3]string{"list_files", `{"path":"src"}`, "TAIL-RESULT d server"},
	)...)
	msgs = append(msgs, turn("TAIL-MARKER 3",
		[3]string{"list_files", `{"path":"tests"}`, "TAIL-RESULT d unit"},
	)...)
	return msgs
}

// B3: the summary is built from the removed prefix's call arguments and
// results, one section each, with no junk.
func TestCheckpointSummaryIsStructured(t *testing.T) {
	msgs := attempt6Transcript()
	var stored []storedCheckpoint
	seq := 0
	wid := uuid.New()
	if !checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "implementation-agent", &seq, true) {
		t.Fatalf("checkpoint did not fire: %s", roles(msgs))
	}
	if msgs[1].Content != "TASK-MARKER: Implement in sample-app: Configure the search API key" {
		t.Fatalf("task not pinned: %.40q", msgs[1].Content)
	}
	sum := msgs[2].Content
	t.Logf("summary:\n%s", sum)
	for _, want := range []string{
		"### Files read\n- src/config/env.ts L1-40\n- src/server/index.ts L30-90\n",
		"### Files edited\n- src/config/env.ts — edit: searchApiKey: process.env.SEARCH_API_KEY || '',\n",
		"### Commands run\n- `npm test` → exit 1: AssertionError: expected '' to be 'test-key'\n",
		`codex_grep "SEARCH" → 2 hits: public/index.html:840, landing/index.html:855`,
		`codex_read_file docs/missing.md → error: not found`,
		"### Decisions\n- I will read the config first.\n",
		"recall_context(",
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	for _, banned := range []string{"{, {", "(non-zero)", `"name":"run_command"`, "TASK-MARKER", "TAIL-MARKER", "TAIL-RESULT"} {
		if strings.Contains(sum, banned) {
			t.Errorf("summary contains %q", banned)
		}
	}
}

// A second checkpoint folds the first summary in: nothing summarised
// earlier is lost, and recall_context is named only when offered.
func TestCheckpointSummaryFoldsTheEarlierSummary(t *testing.T) {
	msgs := attempt6Transcript()
	var stored []storedCheckpoint
	seq := 0
	wid := uuid.New()
	if !checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "implementation-agent", &seq, false) {
		t.Fatal("first checkpoint did not fire")
	}
	msgs = append(msgs, turn("Now the HTML pages.",
		[3]string{"read_file", `{"path":"public/index.html","start_line":1,"end_line":400}`, "<!doctype html>"},
		[3]string{"edit_file", `{"path":"public/index.html","old_string":"a","new_string":"fetch('/api/settings')"}`, "Edit applied to public/index.html: replaced 1 occurrence"},
	)...)
	msgs = append(msgs, turn("x", [3]string{"list_files", `{"path":"a"}`, "a"})...)
	msgs = append(msgs, turn("y", [3]string{"list_files", `{"path":"b"}`, "b"})...)
	msgs = append(msgs, turn("z", [3]string{"list_files", `{"path":"c"}`, "c"})...)
	if !checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "implementation-agent", &seq, false) {
		t.Fatalf("second checkpoint did not fire: %s", roles(msgs))
	}
	sum := msgs[2].Content
	for _, want := range []string{
		"- src/config/env.ts L1-40\n", "- public/index.html L1-400\n",
		"- src/config/env.ts — edit: searchApiKey", "- public/index.html — edit: fetch('/api/settings')",
		"`npm test` → exit 1", "- I will read the config first.", "- Now the HTML pages.",
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("second summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Count(sum, "## Conversation checkpoint") != 1 || strings.Contains(sum, "recall_context") {
		t.Errorf("second summary nests the first or names recall_context:\n%s", sum)
	}
	if n := strings.Count(roles(msgs), "user"); n != 2 {
		t.Errorf("%d user messages, want task + summary", n)
	}
}
