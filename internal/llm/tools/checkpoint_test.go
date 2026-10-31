package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// groupedTranscript is system, user, then groups of one assistant turn with
// perGroup tool calls followed by their results. uniqueIDs selects
// "g<n>_call_<k>" ids; otherwise every group reuses "call_<k>" (some
// providers number calls per turn).
func groupedTranscript(groups []int, uniqueIDs bool) []adapter.ChatMessage {
	msgs := []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "task"},
	}
	for g, n := range groups {
		a := adapter.ChatMessage{Role: adapter.RoleAssistant, Content: fmt.Sprintf("turn %d", g)}
		var results []adapter.ChatMessage
		for k := 0; k < n; k++ {
			id := fmt.Sprintf("call_%d", k)
			if uniqueIDs {
				id = fmt.Sprintf("g%d_call_%d", g, k)
			}
			args := fmt.Sprintf(`{"path":"src/f%d_%d.ts"}`, g, k)
			a.ToolCalls = append(a.ToolCalls, adapter.ToolCall{ID: id, Name: "codex_read_file", Arguments: args})
			results = append(results, adapter.ChatMessage{Role: adapter.RoleTool, ToolCallID: id, Name: "codex_read_file", Content: "body " + id})
		}
		msgs = append(msgs, a)
		msgs = append(msgs, results...)
	}
	return msgs
}

type storedCheckpoint struct {
	seq   int
	turns int
}

func fakeCheckpointStore(out *[]storedCheckpoint) checkpointStore {
	return func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ string, seq int, _ []byte, _ string, turns int) error {
		*out = append(*out, storedCheckpoint{seq: seq, turns: turns})
		return nil
	}
}

// X2: the checkpoint's kept tail used to be the last 6 messages by count,
// which can start in the middle of a tool group (attempt 5:
// system,user,tool×6,assistant,…). The tail must start at a turn boundary
// so every kept tool result follows the assistant turn that called it.
func TestCheckpointKeepsWholeToolGroups(t *testing.T) {
	for _, unique := range []bool{true, false} {
		for _, shape := range [][]int{
			{6, 6, 6, 6, 6}, // 5 assistant+6-tool groups
			{4, 6},          // attempt 5: 14 messages, the last 6 are one group's results
			{3, 5, 2, 7, 1},
		} {
			name := fmt.Sprintf("unique=%t/%v", unique, shape)
			t.Run(name, func(t *testing.T) {
				msgs := groupedTranscript(shape, unique)
				before := len(msgs)
				var stored []storedCheckpoint
				seq := 0
				wid := uuid.New()
				if !checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "research-agent", &seq, false) {
					t.Fatalf("checkpoint did not fire on %d messages", before)
				}
				if len(stored) != 1 || stored[0].turns != before || seq != 1 {
					t.Fatalf("stored = %+v seq=%d", stored, seq)
				}
				if len(msgs) >= before {
					t.Fatalf("checkpoint kept %d of %d messages", len(msgs), before)
				}
				// B2: system, the task, then the summary.
				if msgs[0].Role != adapter.RoleSystem || msgs[1].Content != "task" || !isCheckpointSummary(msgs[2]) {
					t.Fatalf("head = %s, %.20q, %s %.60q", msgs[0].Role, msgs[1].Content, msgs[2].Role, msgs[2].Content)
				}
				if msgs[3].Role == adapter.RoleTool {
					t.Fatalf("the kept tail starts with a tool result: roles %s", roles(msgs))
				}
				assertProtocol(t, msgs)
				// The next request appends the model's turn: still valid.
				assertProtocol(t, append(msgs, adapter.ChatMessage{Role: adapter.RoleAssistant, Content: "done"}))
			})
		}
	}
}

// One group spanning the whole conversation cannot be cut without
// orphaning results: no checkpoint (and nothing stored) instead of a
// broken transcript.
func TestCheckpointSkipsWhenOnlyOneGroupCouldBeKept(t *testing.T) {
	msgs := groupedTranscript([]int{13}, true)
	before := append([]adapter.ChatMessage(nil), msgs...)
	var stored []storedCheckpoint
	seq := 0
	wid := uuid.New()
	if checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "research-agent", &seq, false) {
		t.Fatalf("checkpoint fired: roles %s", roles(msgs))
	}
	if len(stored) != 0 || seq != 0 || len(msgs) != len(before) {
		t.Fatalf("stored=%v seq=%d len=%d", stored, seq, len(msgs))
	}
}

// A user message (a nudge) after the last group is a turn boundary too.
func TestCheckpointTailMayStartAtAUserTurn(t *testing.T) {
	msgs := groupedTranscript([]int{6, 6}, true)
	msgs = append(msgs, adapter.ChatMessage{Role: adapter.RoleUser, Content: "continue"})
	msgs = append(msgs, groupedTranscript([]int{5}, false)[2:]...)
	got := checkpointTailStart(msgs, 6)
	if got < 2 || msgs[got].Role == adapter.RoleTool {
		t.Fatalf("tail start %d (%s)", got, msgs[got].Role)
	}
	assertProtocol(t, msgs[got:])
}

func roles(msgs []adapter.ChatMessage) string {
	var rs []string
	for _, m := range msgs {
		rs = append(rs, string(m.Role))
	}
	return strings.Join(rs, ",")
}

// B2: a checkpoint keeps the system prompt and the task (the first user
// message) verbatim. Live-run attempt 6 checkpointed 8 times and each one
// replaced the 8-step task with a summary that omitted it: from call 5 the
// model never saw steps 3-7.
func TestCheckpointPinsTheTaskThroughEightCheckpoints(t *testing.T) {
	task := "Title: Implement the search key\n\n## Steps\n" + strings.Repeat("1. step text that must survive. ", 40)
	msgs := []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: task},
	}
	var stored []storedCheckpoint
	seq := 0
	wid := uuid.New()
	fired := 0
	for g := 0; fired < 8 && g < 200; g++ {
		msgs = append(msgs, groupedTranscript([]int{2}, false)[2:]...)
		// Every request the loop sends starts with the system prompt and
		// the unchanged task.
		if msgs[0].Content != "system" || msgs[1].Role != adapter.RoleUser || msgs[1].Content != task {
			t.Fatalf("request %d: head is %s %.40q", g, msgs[1].Role, msgs[1].Content)
		}
		if len(msgs) < 14 {
			continue
		}
		if checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "implementation-agent", &seq, false) {
			fired++
			if msgs[1].Content != task {
				t.Fatalf("checkpoint %d replaced the task: %.60q", fired, msgs[1].Content)
			}
			if msgs[2].Role != adapter.RoleUser || !strings.Contains(msgs[2].Content, "onversation checkpoint") && !strings.Contains(msgs[2].Content, "onversation Checkpoint") {
				t.Fatalf("checkpoint %d: message 2 is %s %.60q, want the summary", fired, msgs[2].Role, msgs[2].Content)
			}
			if n := strings.Count(roles(msgs), "user"); n != 2 {
				t.Fatalf("checkpoint %d: %d user messages (%s), want task + one summary", fired, n, roles(msgs))
			}
			assertProtocol(t, msgs)
		}
	}
	if fired != 8 {
		t.Fatalf("checkpoint fired %d times, want 8", fired)
	}
}

// The evidence ledger stays right after the task through a checkpoint.
func TestCheckpointKeepsTheLedgerAfterTheTask(t *testing.T) {
	msgs := groupedTranscript([]int{3, 3, 3, 3}, true)
	ledger := adapter.ChatMessage{Role: adapter.RoleUser, Content: ledgerHeaderParagraph + "- codex_grep(pattern:x) -> 0 results"}
	msgs = append(msgs[:2], append([]adapter.ChatMessage{ledger}, msgs[2:]...)...)
	var stored []storedCheckpoint
	seq := 0
	wid := uuid.New()
	if !checkpoint(context.Background(), fakeCheckpointStore(&stored), &msgs, 50000, 1000, &wid, "implementation-agent", &seq, false) {
		t.Fatal("checkpoint did not fire")
	}
	if msgs[1].Content != "task" || msgs[2].Content != ledger.Content || !strings.Contains(strings.ToLower(msgs[3].Content), "conversation checkpoint") {
		t.Fatalf("head = %q / %.30q / %.30q", msgs[1].Content, msgs[2].Content, msgs[3].Content)
	}
	assertProtocol(t, msgs)
}
