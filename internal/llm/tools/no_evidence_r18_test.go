package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// attempt15Prose is the shape of live-run attempt 15's research answers: a
// plan heading, steps, and the calls as JSON text (several in one fence),
// none of the phrases the prose check looked for.
const attempt15Prose = "### Plan\n1. **Read the specified files** to understand the current implementation.\n2. **Identify importers** of the modules.\n\n### Tool Calls\n\n#### Step 1: Read the specified files\n\n```json\n" +
	`{"name": "codex_read_file", "arguments": {"path": "a.ts", "start_line": 1, "end_line": 5}}` + "\n" +
	`{"name": "codex_read_file", "arguments": {"path": "b.ts"}}` + "\n```"

func proseTurn(s string) adapter.CompletionResponse {
	return adapter.CompletionResponse{Content: s, FinishReason: "stop", InputTokens: 1, OutputTokens: 40}
}

func runScripted(t *testing.T, turns []adapter.CompletionResponse, opts LoopOptions) (*scriptAdapter, *adapter.CompletionResponse, error) {
	t.Helper()
	model := &scriptAdapter{turns: turns}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "s"}, {Role: adapter.RoleUser, Content: "task"}},
		Tools:    []adapter.Tool{ReadFile()},
	}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(cycleTree(t)), 40, nil, nil, opts)
	return model, resp, err
}

// The phrase list does not match attempt 15's answer: that is why the
// prose-planning check did not fire. It now does, on the JSON-text calls.
func TestProsePlanningCheckFiresOnTextToolCalls(t *testing.T) {
	lc := strings.ToLower(attempt15Prose)
	for _, sig := range []string{"let me ", "now let me", "i'll ", "i will ", "next step", "now i need to", "continue with", "let's ", "remaining step", "moving on to"} {
		if strings.Contains(lc, sig) {
			t.Fatalf("fixture contains the phrase %q", sig)
		}
	}
	if !looksLikeUnexecutedPlan(attempt15Prose, false) {
		t.Fatal("JSON-text tool calls are not recognised")
	}
	if !looksLikeUnexecutedPlan("### Plan\n1. Read the files.\n2. Answer.", true) || looksLikeUnexecutedPlan("### Plan\n1. Read the files.", false) {
		t.Fatal("a plan heading counts on the first turn only")
	}
	if looksLikeUnexecutedPlan("AC-1: src/config/env.ts:4 sets the port. The planned change is small.", true) {
		t.Fatal("an ordinary answer is not a plan")
	}

	// An implementation-shaped loop (StuckControl, no EvidenceNudge): the
	// generic prose nudge.
	final := proseTurn("AC-1 is met at a.ts:1.")
	model, resp, err := runScripted(t, []adapter.CompletionResponse{proseTurn(attempt15Prose), final}, LoopOptions{StuckControl: true})
	if err != nil || resp == nil || resp.Content != final.Content || len(model.reqs) != 2 {
		t.Fatalf("calls = %d, resp = %+v, err = %v", len(model.reqs), resp, err)
	}
	if got := userMessages(model.reqs[1], "You described what you plan to do but did not call any tools."); got != 1 {
		t.Fatalf("prose nudges = %d, want 1", got)
	}
}

// R18 review m1: a chat-shaped loop (neither StuckControl nor
// EvidenceNudge) is not nudged for a reply that opens with "## Plan" or
// shows a tool-call example as JSON text: that is its answer.
func TestChatLoopIsNotNudgedForPlansOrCallExamples(t *testing.T) {
	for name, answer := range map[string]string{
		"plan heading": "## Plan\n1. Migrate the table.\n2. Backfill the column.\n3. Switch reads.",
		"call example": "To read a file the agent emits:\n```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n```\nThat is all it needs.",
	} {
		model, resp, err := runScripted(t, []adapter.CompletionResponse{proseTurn(answer), proseTurn("(not reached)")}, LoopOptions{})
		if err != nil || resp == nil || resp.Content != answer || len(model.reqs) != 1 {
			t.Errorf("%s: calls = %d, resp = %+v, err = %v", name, len(model.reqs), resp, err)
		}
	}
	if planTriggers(LoopOptions{}) || !planTriggers(LoopOptions{StuckControl: true}) || !planTriggers(LoopOptions{EvidenceNudge: "x"}) {
		t.Error("planTriggers: research and implementation loops only")
	}
}

// EvidenceNudge: the first tool-less answer is nudged with its text, the
// second ends the loop with NoEvidence carrying the answer.
func TestEvidenceNudgeThenNoEvidence(t *testing.T) {
	const nudge = "you must gather evidence with tools before answering"
	model, resp, err := runScripted(t, []adapter.CompletionResponse{proseTurn(attempt15Prose), proseTurn("AC-1 is probably met.")}, LoopOptions{StuckControl: true, EvidenceNudge: nudge})
	var ne *NoEvidence
	if !errors.As(err, &ne) || !errors.Is(err, ErrNoEvidence) || resp != nil {
		t.Fatalf("err = %v, resp = %+v; want NoEvidence", err, resp)
	}
	if ne.Answers != 2 || ne.Note != "AC-1 is probably met." {
		t.Fatalf("NoEvidence = %+v", *ne)
	}
	if len(model.reqs) != 2 || userMessages(model.reqs[1], nudge) != 1 {
		t.Fatalf("calls = %d; the second request must end with the nudge: %+v", len(model.reqs), model.reqs[len(model.reqs)-1].Messages)
	}
	// INSUFFICIENT before any tool ran is a tool-less answer too.
	_, _, err = runScripted(t, []adapter.CompletionResponse{proseTurn("INSUFFICIENT: no access"), proseTurn("INSUFFICIENT: still none")}, LoopOptions{StuckControl: true, EvidenceNudge: nudge})
	if !errors.As(err, &ne) {
		t.Fatalf("INSUFFICIENT twice before any tool: err = %v, want NoEvidence", err)
	}
}

// Once a tool has run, a final answer is accepted as before, and a loop
// without EvidenceNudge accepts a tool-less answer as before.
func TestEvidenceNudgeOnlyBeforeTheFirstTool(t *testing.T) {
	read := turnsFor([]scriptedCall{linesOfA(1, 5)}, func(int) string { return "" })
	final := proseTurn("AC-1: a.ts:1 exports v1.")
	model, resp, err := runScripted(t, append(read, final), LoopOptions{StuckControl: true, EvidenceNudge: "gather evidence"})
	if err != nil || resp == nil || resp.Content != final.Content || len(model.reqs) != 2 {
		t.Fatalf("calls = %d, resp = %+v, err = %v", len(model.reqs), resp, err)
	}
	model, resp, err = runScripted(t, []adapter.CompletionResponse{final}, LoopOptions{StuckControl: true})
	if err != nil || resp == nil || resp.Content != final.Content || len(model.reqs) != 1 {
		t.Fatalf("no EvidenceNudge: calls = %d, resp = %+v, err = %v", len(model.reqs), resp, err)
	}
}

func TestNoEvidenceNoteIsBounded(t *testing.T) {
	long := strings.Repeat("é", 3000)
	if got := boundNote(long); len(got) > noEvidenceNoteMax+len("…") || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "é") {
		t.Fatalf("bound note: %d bytes", len(got))
	}
}
