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
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/google/uuid"
)

// R10 replay of attempt 9's shape: a synthetic model transcript
// (testdata/attempt9/transcript.json, modelled on a live run's 45 calls and
// rewritten onto the demo-app fixture) is fed to a fake adapter in order
// while the real implementation loop and tool executor run against a temp
// copy of the demo-app files it edits. The model does not see our results,
// so the replay measures how early the loop stops a model that keeps
// re-issuing the same duplicating edit.

type attempt9Transcript struct {
	LoopSystem string `json:"loop1_system"`
	LoopTask   string `json:"loop1_task"`
	ContTask   string `json:"continuation_task"`
	Calls      []struct {
		Call         int    `json:"call"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Response     string `json:"response_content"`
		ToolCalls    []struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"tool_calls"`
	} `json:"calls"`
}

func loadAttempt9(t *testing.T) attempt9Transcript {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt9/transcript.json")
	if err != nil {
		t.Fatal(err)
	}
	var tr attempt9Transcript
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

// attempt9Files are the subtask's files in scope.
var attempt9Files = []string{"src/config/env.ts", "src/server/index.ts", "public/index.html", "landing/index.html", "README.md", ".env.example", "docker-compose.yml", "docs/configuration.md", "tests/unit/server.test.ts"}

// copyAttempt9Tree copies the demo-app fixture (LF) into a temp dir.
func copyAttempt9Tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range attempt9Files {
		b, err := os.ReadFile(filepath.Join("testdata/attempt9/demo-app", filepath.FromSlash(rel)))
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

// replayAdapter serves recorded calls first..last in order.
type replayAdapter struct {
	tr      attempt9Transcript
	next    int // index into tr.Calls
	last    int // index of the last call served by this replay
	served  []int
	reqs    []adapter.CompletionRequest
	outToks int
}

func (a *replayAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	cp.Tools = append([]adapter.Tool(nil), req.Tools...)
	a.reqs = append(a.reqs, cp)
	if a.next > a.last {
		return &adapter.CompletionResponse{Content: "(replay exhausted)", FinishReason: "stop"}, nil
	}
	c := a.tr.Calls[a.next]
	a.next++
	a.served = append(a.served, c.Call)
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
func (a *replayAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *replayAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *replayAdapter) HealthCheck(context.Context) error { return nil }
func (a *replayAdapter) Name() string                      { return "replay/attempt9" }

// replayExecutor is the real executor on root, with the tools that need the
// outside world (npm, the Codex index) answered as attempt 9's were.
func replayExecutor(root string) *tools.Executor {
	ex := tools.NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		switch call.Name {
		case "run_command":
			return adapter.ToolResult{Content: "> demo-app@0.1.0 test\n> vitest run\n\n Test Files  10 passed (10)\n      Tests  58 passed (58)\n"}, true
		case "codex_grep":
			return adapter.ToolResult{Content: `{"hits":[],"total_hits":0,"truncated":false}`}, true
		case "codex_verify_imports":
			return adapter.ToolResult{Content: `{"checked":4,"path":"tests/unit/server.test.ts","repo":"demo-app","unresolved":null}`}, true
		}
		return adapter.ToolResult{}, false
	})
	return ex
}

type replayResult struct {
	calls     int
	estTokens int // chars/4 of every request's input + the recorded output tokens
	recorded  int // attempt 9's recorded in+out tokens of the calls served
	err       error
	served    []int
}

// replayLoop runs the implementation loop over recorded calls first..last
// (1-based) with task as the task message.
func replayLoop(t *testing.T, tr attempt9Transcript, root, task string, first, last int, opts func(*tools.LoopOptions)) replayResult {
	t.Helper()
	model := &replayAdapter{tr: tr, next: first - 1, last: last - 1}
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: tr.LoopSystem},
			{Role: adapter.RoleUser, Content: task},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	o := implementationLoopOptions(req.Tools, func() bool { return false }, attempt9Files)
	// The live loop checkpoints (it has a database); so does the replay.
	o.CheckpointStore = func(context.Context, uuid.UUID, uuid.UUID, string, int, []byte, string, int) error { return nil }
	if opts != nil {
		opts(&o)
	}
	_, err := tools.RunToolLoopWithOptions(context.Background(), model, req, replayExecutor(root), 60, nil, nil, o)
	res := replayResult{calls: len(model.reqs), err: err, served: model.served}
	for _, r := range model.reqs {
		res.estTokens += tools.EstimatePromptTokens(r)
	}
	res.estTokens += model.outToks
	for _, n := range model.served {
		c := tr.Calls[n-1]
		res.recorded += c.InputTokens + c.OutputTokens
	}
	return res
}

// R10 acceptance: replaying attempt 9's first implementation loop, the loop
// ends with LoopStuck on tests/unit/server.test.ts by call 20 and within
// 150k estimated tokens (attempt 9: 26 calls, 206,933 tokens, then a
// continuation that re-ran the same failing edit for 109k).
func TestReplayAttempt9LoopEndsStuckEarly(t *testing.T) {
	tr := loadAttempt9(t)
	root := copyAttempt9Tree(t)
	res := replayLoop(t, tr, root, tr.LoopTask, 1, 26, nil)
	t.Logf("attempt-9 replay, loop 1: %d LLM calls (recorded calls %v), %d est. tokens (attempt 9 recorded %d for the same calls); err: %v",
		res.calls, res.served, res.estTokens, res.recorded, res.err)
	var stuck *tools.LoopStuck
	if !errors.As(res.err, &stuck) {
		t.Fatalf("loop did not end with LoopStuck: %v", res.err)
	}
	if stuck.Path != "tests/unit/server.test.ts" || stuck.Tool != "edit_file" || stuck.Attempts < 3 || stuck.Reason == "" {
		t.Fatalf("LoopStuck = %+v", *stuck)
	}
	if res.calls > 20 {
		t.Fatalf("loop ran %d calls, want <= 20", res.calls)
	}
	if res.estTokens > 150000 {
		t.Fatalf("loop cost %d est. tokens, want <= 150000", res.estTokens)
	}
}

// R10 W4 on the replay: the continuation (recorded calls 27-45) runs on
// the tree loop 1 left, with loop 1's LoopStuck inherited. Call 35's clean
// test insertion lands (W1), the re-issued compose/docs edits get
// "already applied" (W2, no duplicated lines), and the pass ends early.
func TestReplayAttempt9Continuation(t *testing.T) {
	tr := loadAttempt9(t)
	root := copyAttempt9Tree(t)
	first := replayLoop(t, tr, root, tr.LoopTask, 1, 26, nil)
	var stuck *tools.LoopStuck
	if !errors.As(first.err, &stuck) {
		t.Fatalf("loop 1: %v", first.err)
	}
	changed := map[string]bool{"src/config/env.ts": true, "src/server/index.ts": true, "public/index.html": true, "landing/index.html": true}
	var missing []string
	for _, f := range attempt9Files {
		if !changed[f] {
			missing = append(missing, f)
		}
	}
	task := tr.LoopTask + "\n\n## Continuation\n" + continuationPrompt(changed, missing, []tools.LoopStuck{*stuck})
	cont := replayLoop(t, tr, root, task, 27, 45, func(o *tools.LoopOptions) { o.PriorStuck = []tools.LoopStuck{*stuck} })
	t.Logf("attempt-9 replay, continuation: %d LLM calls (recorded %v), %d est. tokens (attempt 9 recorded %d for the same calls); err: %v",
		cont.calls, cont.served, cont.estTokens, cont.recorded, cont.err)
	t.Logf("attempt-9 replay total: %d calls, %d est. tokens (attempt 9: 45 calls, 376,903 tokens)", first.calls+cont.calls, first.estTokens+cont.estTokens)
	test, _ := os.ReadFile(filepath.Join(root, "tests/unit/server.test.ts"))
	if !strings.Contains(string(test), "describe('GET /api/client-config'") {
		t.Fatal("the client-config test (call 35) did not land")
	}
	for _, f := range []string{"docker-compose.yml", "docs/configuration.md", ".env.example"} {
		b, _ := os.ReadFile(filepath.Join(root, f))
		if n := strings.Count(string(b), "TILER_API_KEY"); n > 1 {
			t.Errorf("%s has %d TILER_API_KEY lines (a re-applied edit duplicated it)", f, n)
		}
	}
	if cont.calls > 19 {
		t.Fatalf("continuation ran %d calls, recorded 19", cont.calls)
	}
}
