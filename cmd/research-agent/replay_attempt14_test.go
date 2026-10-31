package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// R17 replay of live run attempt 14 (work item 8386046c): the research
// loops' recorded model responses (inference_logs export in
// testdata/attempt14, rebuilt on the synthetic sample-app) are fed to a
// fake adapter in order while the real research loop options and tool
// executor run against the sample-app files. The impact assessment loop is
// the one that cycled public/index.html 836-853 → landing/index.html 855 →
// src/server/index.ts 33-60 for 25 turns.

type attempt14Loop struct {
	System string `json:"system"`
	User   string `json:"user"`
	Calls  []struct {
		Call         int    `json:"call"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Response     string `json:"response_content"`
		ToolCalls    []struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"tool_calls"`
		ToolResults []string `json:"tool_results"`
	} `json:"calls"`
}

func loadAttempt14(t *testing.T) map[string]attempt14Loop {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt14/research_transcript.json")
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Loops map[string]attempt14Loop `json:"loops"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Loops
}

// sampleAppFiles are the files attempt 14's research read; the tree is the
// research coordinator's sample-app fixture.
var sampleAppFiles = []string{"src/config/env.ts", "src/server/index.ts", "public/index.html", "landing/index.html", "README.md", ".env.example", "docker-compose.yml", "docs/configuration.md", "tests/unit/server.test.ts"}

func copySampleAppTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range sampleAppFiles {
		b, err := os.ReadFile(filepath.Join("..", "research-coordinator", "testdata", "sample-app", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(strings.ReplaceAll(string(b), "\r\n", "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// recordedAdapter serves a loop's recorded responses in order.
type recordedAdapter struct {
	loop    attempt14Loop
	next    int
	reqs    []adapter.CompletionRequest
	outToks int
}

func (a *recordedAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	a.reqs = append(a.reqs, cp)
	if a.next >= len(a.loop.Calls) {
		return &adapter.CompletionResponse{Content: "(replay exhausted)", FinishReason: "stop"}, nil
	}
	c := a.loop.Calls[a.next]
	a.next++
	a.outToks += c.OutputTokens
	resp := &adapter.CompletionResponse{Content: c.Response, FinishReason: "stop", InputTokens: c.InputTokens, OutputTokens: c.OutputTokens}
	for i, tc := range c.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, adapter.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: tc.Name, Arguments: tc.Arguments})
	}
	if len(resp.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}
func (a *recordedAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *recordedAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *recordedAdapter) HealthCheck(context.Context) error { return nil }
func (a *recordedAdapter) Name() string                      { return "replay/attempt14" }

// recordedExecutor is the real executor on root; the Codex graph and
// vector tools answer as attempt 14's did.
func recordedExecutor(root string, loop attempt14Loop) *tools.Executor {
	recorded := map[string]string{}
	for _, c := range loop.Calls {
		for k, tc := range c.ToolCalls {
			if k < len(c.ToolResults) {
				recorded[tc.Name+"\x00"+tc.Arguments] = c.ToolResults[k]
			}
		}
	}
	ex := tools.NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		switch call.Name {
		case "codex_find_module", "codex_dependents_of", "codex_dependencies_of", "codex_similar_code", "codex_grep", "codex_list_tree":
			if r, ok := recorded[call.Name+"\x00"+call.Arguments]; ok {
				return adapter.ToolResult{Content: r}, true
			}
			return adapter.ToolResult{Content: `{"results":[]}`}, true
		}
		return adapter.ToolResult{}, false
	})
	return ex
}

type attempt14Replay struct {
	calls     int
	estTokens int // chars/4 of every request + the recorded output tokens
	recorded  int // attempt 14's recorded in+out tokens of the calls served
	err       error
	reqs      []adapter.CompletionRequest
}

func replayResearch(t *testing.T, researchType string, opts func(*tools.LoopOptions)) attempt14Replay {
	t.Helper()
	loop := loadAttempt14(t)[researchType]
	if len(loop.Calls) == 0 {
		t.Fatalf("no recorded %s loop", researchType)
	}
	model := &recordedAdapter{loop: loop}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: loop.System},
			{Role: adapter.RoleUser, Content: loop.User},
		},
		Tools: researchToolSet(researchType, false),
	}
	o := researchLoopOptions(researchType, func() bool { return false })
	if opts != nil {
		opts(&o)
	}
	maxIters := strategy.PromptScaffolding(models.OperatingModeStandard).MaxIterations
	_, err := tools.RunToolLoopWithOptions(context.Background(), model, req, recordedExecutor(copySampleAppTree(t), loop), maxIters, nil, nil, o)
	res := attempt14Replay{calls: len(model.reqs), err: err, reqs: model.reqs}
	for _, r := range model.reqs {
		res.estTokens += tools.EstimatePromptTokens(r)
	}
	res.estTokens += model.outToks
	for _, c := range loop.Calls[:min(model.next, len(loop.Calls))] {
		res.recorded += c.InputTokens + c.OutputTokens
	}
	return res
}

// R17 L1/L2 acceptance: the impact-assessment loop that cycled three reads
// for 25 turns (31 calls, 176,307 recorded tokens) ends by call 12 with a
// structured LoopStuck, within 80k estimated tokens.
func TestReplayAttempt14ResearchCycleEndsEarly(t *testing.T) {
	res := replayResearch(t, "impact_assessment", nil)
	t.Logf("attempt-14 impact_assessment replay: %d LLM calls, %d est. tokens (attempt 14 recorded %d for the same calls; whole loop 31 calls, 176,307); err: %v",
		res.calls, res.estTokens, res.recorded, res.err)
	var stuck *tools.LoopStuck
	if !errors.As(res.err, &stuck) {
		t.Fatalf("loop did not end with LoopStuck: %v", res.err)
	}
	if res.calls > 12 {
		t.Fatalf("loop ran %d calls, want <= 12", res.calls)
	}
	if res.estTokens > 80000 {
		t.Fatalf("loop cost %d est. tokens, want <= 80000", res.estTokens)
	}
	if stuck.Tool != "codex_read_file" || stuck.Path == "" || stuck.Kind == "" {
		t.Fatalf("LoopStuck = %+v", *stuck)
	}
}

// The code_trace loop of the same item read nine different ranges and
// answered: it still runs to its final answer.
func TestReplayAttempt14CodeTraceIsUnchanged(t *testing.T) {
	res := replayResearch(t, "code_trace", nil)
	t.Logf("attempt-14 code_trace replay: %d LLM calls, %d est. tokens (recorded %d); err: %v", res.calls, res.estTokens, res.recorded, res.err)
	if res.err != nil {
		t.Fatalf("code_trace replay failed: %v", res.err)
	}
	if res.calls != 11 {
		t.Fatalf("calls = %d, want the recorded 11", res.calls)
	}
}

// The replay's repeats are answered as ReadOnly says: the first repeat of
// a read keeps its content with a note, the second is one line.
func TestReplayAttempt14RepeatsAreAnswered(t *testing.T) {
	res := replayResearch(t, "impact_assessment", nil)
	short := 0
	for _, m := range res.reqs[len(res.reqs)-1].Messages {
		if m.Role == adapter.RoleTool && strings.Contains(m.Content, "unchanged since your last read of lines") && strings.Contains(m.Content, "ends this task as stuck") {
			short++
		}
	}
	// The last request holds the latest two turns in full (KeepRecent 2):
	// calls 10 and 11 were second repeats (public 836-853, landing 855).
	if short != 2 {
		t.Fatalf("short answers in the last request = %d, want 2", short)
	}
	found := false
	for _, m := range res.reqs[7].Messages { // after call 7: public/index.html, first repeat
		if m.Role == adapter.RoleTool && strings.Contains(m.Content, "note_repeat") && strings.Contains(m.Content, "night_base") {
			found = true
		}
	}
	if !found {
		t.Fatal("the first repeat of public/index.html 836-853 does not keep its content with the note")
	}
}

// R17 L2 on its own (no ReadOnly): the whole-turn cycle detector warns once
// when the last 9 turns are the 3 reads 3 times each, and ends the loop 3
// turns later with a cycle LoopStuck.
func TestReplayAttempt14CycleDetectorAloneStops(t *testing.T) {
	res := replayResearch(t, "impact_assessment", func(o *tools.LoopOptions) { o.ReadOnly = false })
	t.Logf("attempt-14 impact_assessment replay, cycle detector only: %d LLM calls, %d est. tokens (recorded %d); err: %v", res.calls, res.estTokens, res.recorded, res.err)
	var stuck *tools.LoopStuck
	if !errors.As(res.err, &stuck) || stuck.Kind != tools.StuckKindCycle {
		t.Fatalf("err = %v, want a cycle LoopStuck", res.err)
	}
	warnings := 0
	for _, m := range res.reqs[len(res.reqs)-1].Messages {
		if m.Role == adapter.RoleUser && strings.HasPrefix(m.Content, "CYCLE: you have read the same 3 range(s) 3 times") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("cycle warnings = %d, want 1", warnings)
	}
	if res.calls > 17 {
		t.Fatalf("loop ran %d calls, want <= 17", res.calls)
	}
}

// R17 L1: research loops run with the stuck control and re-read dedup, on a
// read-only tree.
func TestResearchLoopOptionsStopStuckLoops(t *testing.T) {
	o := researchLoopOptions("code_trace", func() bool { return false })
	if !o.StuckControl || !o.DedupReads || !o.ReadOnly || !o.EvidenceLedger || !o.DeadToolGuard || o.EvidenceNudge == "" {
		t.Fatalf("options = %+v", o)
	}
}

// A research LoopStuck is a structured task failure (status loop_stuck,
// with the LoopStuck in the result) for the coordinator's honest-failure
// gate, not an error string.
func TestResearchLoopStuckIsAStructuredFailure(t *testing.T) {
	s := &tools.LoopStuck{Tool: "codex_read_file", Path: "src/server/index.ts", Kind: tools.StuckKindRepeatedRead, Attempts: 4, Reason: "repeated read"}
	r := loopFailureResult(testTask("impact_assessment"), fmt.Errorf("tool loop: %w", s))
	if r.Status != ResearchStatusLoopStuck || r.LoopStuck == nil || r.LoopStuck.Kind != tools.StuckKindRepeatedRead || r.Confidence != 0 {
		t.Fatalf("result = %+v", r)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"status":"loop_stuck"`) || !strings.Contains(string(b), `"loop_stuck":{"tool":"codex_read_file","path":"src/server/index.ts","kind":"repeated_read"`) {
		t.Fatalf("wire form: %s", b)
	}
	if r := loopFailureResult(testTask("code_trace"), fmt.Errorf("%w: 40 iterations", tools.ErrSafetyLimit)); r.Status != ResearchStatusLimitHit || r.LoopStuck != nil {
		t.Fatalf("safety limit result = %+v", r)
	}
}
