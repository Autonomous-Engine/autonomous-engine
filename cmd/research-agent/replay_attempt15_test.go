package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// R18 E1 replay of live run attempt 15 (work item 59bd88f8): each research
// task's single recorded response (inference_logs export in
// testdata/attempt15, rebuilt on the synthetic sample-app) was a prose "### Plan … #### Step 1 …" whose tool
// calls were JSON text in a fence; the loop ran no tool and the task was
// recorded status=ok. Research must not succeed without evidence.

type attempt15Loop struct {
	System       string `json:"system"`
	User         string `json:"user"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Response     string `json:"response_content"`
}

func loadAttempt15(t *testing.T) map[string]attempt15Loop {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt15/research_responses.json")
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Loops map[string]attempt15Loop `json:"loops"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Loops
}

// turnScript answers with the scripted responses in order.
type turnScript struct {
	resps []*adapter.CompletionResponse
	reqs  []adapter.CompletionRequest
}

func (a *turnScript) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	a.reqs = append(a.reqs, cp)
	if len(a.reqs) > len(a.resps) {
		return &adapter.CompletionResponse{Content: "(script exhausted)", FinishReason: "stop"}, nil
	}
	return a.resps[len(a.reqs)-1], nil
}
func (a *turnScript) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *turnScript) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *turnScript) HealthCheck(context.Context) error { return nil }
func (a *turnScript) Name() string                      { return "replay/attempt15" }

// attempt15Task is the research task attempt 15 dispatched, with its seeds.
func attempt15Task(researchType string) researchTaskMessage {
	task := testTask(researchType)
	task.WorkID = "59bd88f8-3b7f-4bb3-9739-636b21b40d67"
	task.SeedFiles = []string{"public/index.html", "src/server/index.ts", "landing/index.html", "src/config/env.ts", "docs/configuration.md", "tests/unit/server.test.ts", "README.md", ".env.example", "docker-compose.yml"}
	task.SeedLines = map[string][]int{"public/index.html": {836, 853}, "src/server/index.ts": {33, 60}, "landing/index.html": {855}}
	return task
}

func proseAnswer(l attempt15Loop) *adapter.CompletionResponse {
	return &adapter.CompletionResponse{Content: l.Response, FinishReason: "stop", InputTokens: l.InputTokens, OutputTokens: l.OutputTokens}
}

// runAttempt15 runs the research loop as the handler does (options, tools,
// executor on the sample-app tree) on the scripted responses.
func runAttempt15(t *testing.T, researchType string, resps ...*adapter.CompletionResponse) (*turnScript, *adapter.CompletionResponse, error) {
	t.Helper()
	loop := loadAttempt15(t)[researchType]
	model := &turnScript{resps: resps}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: loop.System},
			{Role: adapter.RoleUser, Content: loop.User},
		},
		Tools: researchToolSet(researchType, false),
	}
	ex := tools.NewExecutor(copySampleAppTree(t))
	ex.SetCodexReadsFromWorktree(true)
	maxIters := strategy.PromptScaffolding(models.OperatingModeStandard).MaxIterations
	resp, err := tools.RunToolLoopWithOptions(context.Background(), model, req, ex, maxIters, nil, nil,
		researchTaskLoopOptions(attempt15Task(researchType), func() bool { return false }))
	return model, resp, err
}

// The first prose answer gets the evidence nudge; a second prose answer
// ends the task as no_evidence with the prose kept as its note.
func TestReplayAttempt15ProseAnswersEndAsNoEvidence(t *testing.T) {
	for _, rt := range []string{"code_trace", "impact_assessment"} {
		t.Run(rt, func(t *testing.T) {
			loop := loadAttempt15(t)[rt]
			model, _, err := runAttempt15(t, rt, proseAnswer(loop), proseAnswer(loop))
			if len(model.reqs) != 2 {
				t.Fatalf("LLM calls = %d, want 2 (the prose answer, then one nudge); err %v", len(model.reqs), err)
			}
			nudge := model.reqs[1].Messages[len(model.reqs[1].Messages)-1]
			if nudge.Role != adapter.RoleUser || !strings.Contains(nudge.Content, "you must gather evidence with tools before answering") ||
				!strings.Contains(nudge.Content, "codex_read_file") || !strings.Contains(nudge.Content, "public/index.html") {
				t.Fatalf("second call does not end with the evidence nudge: %+v", nudge)
			}
			if err == nil {
				t.Fatal("two prose answers ended the loop without an error (would be status=ok)")
			}
			r := loopFailureResult(attempt15Task(rt), err)
			if r.Status != "no_evidence" || r.Confidence != 0 {
				t.Fatalf("result status = %q (err %v), want no_evidence", r.Status, err)
			}
			if !strings.Contains(r.Note, "### Plan") || !strings.Contains(r.Note, "codex_read_file") {
				t.Fatalf("the prose is not kept as the note: %q", r.Note)
			}
			b, _ := json.Marshal(r)
			if !strings.Contains(string(b), `"status":"no_evidence"`) || !strings.Contains(string(b), `"note":`) {
				t.Fatalf("wire form: %.300s", b)
			}
		})
	}
}

// After the nudge a response with tool calls proceeds: the reads run and
// the answer that follows is the task's result.
func TestReplayAttempt15ToolCallsAfterTheNudgeProceed(t *testing.T) {
	loop := loadAttempt15(t)["code_trace"]
	read := &adapter.CompletionResponse{FinishReason: "tool_calls", ToolCalls: []adapter.ToolCall{
		{ID: "call_0", Name: "codex_read_file", Arguments: `{"path":"src/config/env.ts"}`},
	}}
	final := &adapter.CompletionResponse{Content: "AC-1: src/config/env.ts:1 has no mapforgeApiKey yet.", FinishReason: "stop"}
	model, resp, err := runAttempt15(t, "code_trace", proseAnswer(loop), read, final)
	if err != nil {
		t.Fatalf("loop failed: %v", err)
	}
	if len(model.reqs) != 3 || resp == nil || !strings.Contains(resp.Content, "src/config/env.ts:1") {
		t.Fatalf("calls = %d, resp = %+v", len(model.reqs), resp)
	}
	// The read ran: its result is in the last request.
	ran := false
	for _, m := range model.reqs[2].Messages {
		if m.Role == adapter.RoleTool && strings.Contains(m.Content, "env.ts") {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the codex_read_file call after the nudge did not run")
	}
}

// A first response with tool calls is never nudged.
func TestReplayAttempt15ToolCallsFirstAreNotNudged(t *testing.T) {
	read := &adapter.CompletionResponse{FinishReason: "tool_calls", ToolCalls: []adapter.ToolCall{
		{ID: "call_0", Name: "codex_read_file", Arguments: `{"path":"src/config/env.ts"}`},
	}}
	final := &adapter.CompletionResponse{Content: "AC-1: src/config/env.ts:1.", FinishReason: "stop"}
	model, _, err := runAttempt15(t, "impact_assessment", read, final)
	if err != nil || len(model.reqs) != 2 {
		t.Fatalf("calls = %d, err = %v", len(model.reqs), err)
	}
	for _, r := range model.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "you must gather evidence with tools before answering") {
				t.Fatal("a loop that called a tool was nudged")
			}
		}
	}
}

// The backstop after the loop: an answer resting on no successful tool
// call (every call failed) is no_evidence, not ok; one good call is enough.
func TestNoEvidenceResultNeedsASuccessfulCall(t *testing.T) {
	task := attempt15Task("code_trace")
	failed := []toolCallRecord{{tool: "codex_read_file", arguments: `{"path":"nope.ts"}`, result: "file not found", isError: true}}
	r, bad := noEvidenceResult(task, "AC-1 is met.", failed)
	if !bad || r.Status != ResearchStatusNoEvidence || r.Note != "AC-1 is met." || !strings.Contains(r.ErrorDetail, "all failed") || r.Confidence != 0 {
		t.Fatalf("all calls failed: %+v (failed=%v)", r, bad)
	}
	if r, bad := noEvidenceResult(task, "x", nil); !bad || !strings.Contains(r.ErrorDetail, "without running a tool call") {
		t.Fatalf("no calls: %+v", r)
	}
	good := append(failed, toolCallRecord{tool: "codex_read_file", arguments: `{"path":"src/config/env.ts"}`, result: "1: export const env = {"})
	if _, bad := noEvidenceResult(task, "AC-1 is met at src/config/env.ts:1.", good); bad {
		t.Fatal("a successful call was not accepted as evidence")
	}
}

// The handler builds its loop options per task, so the nudge names the
// task's seeds; without seeds it names the graph tool.
func TestEvidenceNudgeNamesTheSeeds(t *testing.T) {
	o := researchTaskLoopOptions(attempt15Task("code_trace"), func() bool { return false })
	if !strings.Contains(o.EvidenceNudge, "codex_read_file on the seeds now (public/index.html, src/server/index.ts, landing/index.html, src/config/env.ts, docs/configuration.md)") ||
		strings.Contains(o.EvidenceNudge, "README.md") || !o.StuckControl || !o.ReadOnly {
		t.Fatalf("options: %+v", o)
	}
	if n := evidenceNudge(nil); !strings.Contains(n, "codex_find_module or codex_read_file") || !strings.Contains(n, "not JSON written in your reply") {
		t.Fatalf("seedless nudge: %q", n)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(src), "\r\n", "\n"), "researchTaskLoopOptions(task, func() bool { return executor.LSPReady(msgCtx) })") {
		t.Fatal("the handler does not build its loop options with researchTaskLoopOptions")
	}
}
