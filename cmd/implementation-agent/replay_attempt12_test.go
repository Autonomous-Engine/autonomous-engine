package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// R12 replay of attempt 12's implementation main pass: the tool calls of
// turns 1-12 (testdata/attempt12/replay_turns.json, modelled on a live
// run's calls 2-13 and rewritten onto the demo-app fixture) are issued in
// order by a fake model while the real loop and executor run on the
// demo-app files (testdata/attempt9/demo-app). The model does not see our
// results; the replay measures the read results calls 2-13 carry, against
// the read-result sizes the live run recorded.

type attempt12Replay struct {
	Turns [][]struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"turns"`
	RecordedReadChars []int          `json:"recorded_read_result_chars"`
	RecordedInput     []int          `json:"recorded_input_tokens"`
	SeedLines         map[string]int `json:"seed_lines"`
	Scope             []string       `json:"scope"`
}

func loadAttempt12Replay(t *testing.T) attempt12Replay {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt12/replay_turns.json")
	if err != nil {
		t.Fatal(err)
	}
	var r attempt12Replay
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// turnsAdapter issues one recorded turn per call, then a final answer.
type turnsAdapter struct {
	turns [][]adapter.ToolCall
	reqs  []adapter.CompletionRequest
}

func (a *turnsAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	a.reqs = append(a.reqs, cp)
	if n := len(a.reqs) - 1; n < len(a.turns) {
		return &adapter.CompletionResponse{Content: fmt.Sprintf("turn %d", n+1), ToolCalls: a.turns[n], FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 100}, nil
	}
	return &adapter.CompletionResponse{Content: "Done: changed the files.", FinishReason: "stop", InputTokens: 1, OutputTokens: 20}, nil
}
func (a *turnsAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *turnsAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *turnsAdapter) HealthCheck(context.Context) error { return nil }
func (a *turnsAdapter) Name() string                      { return "replay/attempt12" }

// readResultChars is what one request carries of read results: every
// read tool message plus the ledger's "Current file contents".
func readResultChars(msgs []adapter.ChatMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == adapter.RoleTool && (m.Name == "read_file" || m.Name == "codex_read_file") {
			n += len(m.Content)
		}
		if m.Role == adapter.RoleUser && strings.HasPrefix(m.Content, "## Evidence ledger") {
			if k := strings.Index(m.Content, "## Current file contents"); k >= 0 {
				n += len(m.Content) - k
			}
		}
	}
	return n
}

// replayAttempt12 runs the 12 recorded turns and returns the read-result
// chars of requests 2-13 (the model's calls 2-13) and their estimated
// input tokens.
func replayAttempt12(t *testing.T, rp attempt12Replay, dedup bool) (chars []int, tokens []int, short, pinned int) {
	t.Helper()
	var turns [][]adapter.ToolCall
	for _, tc := range rp.Turns {
		var calls []adapter.ToolCall
		for i, c := range tc {
			calls = append(calls, adapter.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: c.Name, Arguments: c.Arguments})
		}
		turns = append(turns, calls)
	}
	model := &turnsAdapter{turns: turns}
	ex := replayExecutor(copyAttempt9Tree(t))
	ex.SetReadSeeds(rp.SeedLines)
	// Count the short "unchanged" answers (X2) and those a pin backs.
	ex.SetCallback(func(_, _, _, result string, _ bool, _ tools.ResultMeta) {
		if strings.Contains(result, ": unchanged since your last read of lines ") {
			short++
			if strings.Contains(result, "(pinned)") {
				pinned++
			}
		}
	})
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: "Task: add the Tiler API key (attempt 12 replay)"},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	o := implementationLoopOptions(req.Tools, func() bool { return false }, rp.Scope)
	o.CheckpointStore = func(context.Context, uuid.UUID, uuid.UUID, string, int, []byte, string, int) error { return nil }
	o.DedupReads = dedup
	if _, err := tools.RunToolLoopWithOptions(context.Background(), model, req, ex, 60, nil, nil, o); err != nil {
		t.Logf("loop ended: %v", err)
	}
	for k := 1; k < len(model.reqs) && k <= 12; k++ {
		chars = append(chars, readResultChars(model.reqs[k].Messages))
		tokens = append(tokens, tools.EstimatePromptTokens(model.reqs[k]))
	}
	return chars, tokens, short, pinned
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// R12 acceptance (X1 + X2): replaying attempt 12's reads, the read results
// calls 2-13 carry drop by at least half.
func TestReplayAttempt12ReadsShrink(t *testing.T) {
	rp := loadAttempt12Replay(t)
	if len(rp.Turns) != 12 || len(rp.RecordedReadChars) != 12 {
		t.Fatalf("fixture: %d turns, %d recorded calls", len(rp.Turns), len(rp.RecordedReadChars))
	}
	// Before: 400-line windows (no char cap) and no re-read dedup, as at
	// 921fef9.
	t.Setenv("READ_RESULT_MAX_CHARS", "100000000")
	beforeChars, beforeTok, _, _ := replayAttempt12(t, rp, false)
	// After: the defaults (4,800 chars, dedup on).
	t.Setenv("READ_RESULT_MAX_CHARS", "")
	afterChars, afterTok, short, pinned := replayAttempt12(t, rp, true)
	t.Logf("short answers: %d, of them (pinned): %d", short, pinned)
	if pinned == 0 {
		t.Error("no re-read was answered from a pin")
	}
	if len(beforeChars) != 12 || len(afterChars) != 12 {
		t.Fatalf("calls: before %d, after %d", len(beforeChars), len(afterChars))
	}
	t.Logf("attempt 12 recorded read-result chars, calls 2-13: %v = %d (input tokens %d)", rp.RecordedReadChars, sum(rp.RecordedReadChars), sum(rp.RecordedInput))
	t.Logf("replay before (921fef9 behaviour): %v = %d chars, %d est. tokens", beforeChars, sum(beforeChars), sum(beforeTok))
	t.Logf("replay after (X1+X2): %v = %d chars, %d est. tokens", afterChars, sum(afterChars), sum(afterTok))
	t.Logf("drop: %.1f%% of the replayed before, %.1f%% of the recorded",
		100*(1-float64(sum(afterChars))/float64(sum(beforeChars))), 100*(1-float64(sum(afterChars))/float64(sum(rp.RecordedReadChars))))
	if 2*sum(afterChars) > sum(beforeChars) {
		t.Fatalf("read-result chars %d after vs %d before: less than a 50%% drop", sum(afterChars), sum(beforeChars))
	}
	// The replayed "before" is the recorded run within 10%.
	if b, r := float64(sum(beforeChars)), float64(sum(rp.RecordedReadChars)); b < 0.9*r || b > 1.1*r {
		t.Errorf("replayed before %d chars, recorded %d", sum(beforeChars), sum(rp.RecordedReadChars))
	}
}
