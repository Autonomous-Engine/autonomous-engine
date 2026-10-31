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
)

// floorRunAdapter replays scripted tool-call turns and records every request.
type floorRunAdapter struct {
	turns []adapter.CompletionResponse
	n     int
	reqs  []adapter.CompletionRequest
}

func (a *floorRunAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	cp.Tools = append([]adapter.Tool(nil), req.Tools...)
	a.reqs = append(a.reqs, cp)
	if a.n < len(a.turns) {
		t := a.turns[a.n]
		a.n++
		return &t, nil
	}
	return &adapter.CompletionResponse{Content: "Changed src/config/env.ts, src/server/index.ts, both HTML pages, the docs and the test.", FinishReason: "stop", OutputTokens: 40}, nil
}
func (a *floorRunAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *floorRunAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *floorRunAdapter) HealthCheck(context.Context) error { return nil }
func (a *floorRunAdapter) Name() string                      { return "replay/test" }

// demoAppTree writes files shaped like the demo-app fixture's (the nine
// files in scope): n numbered lines of about 38 chars each.
func demoAppTree(t *testing.T) string {
	root := t.TempDir()
	for rel, n := range map[string]int{
		"src/config/env.ts": 40, "src/server/index.ts": 300, "public/index.html": 900, "landing/index.html": 900,
		"README.md": 120, ".env.example": 12, "docker-compose.yml": 40, "docs/configuration.md": 80, "tests/unit/server.test.ts": 60,
	} {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "// %s line %03d of the original file\n", filepath.Base(rel), i)
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// R6 acceptance: a 20-iteration run of the main implementation loop on
// attempt 6's task — reads, edits and noisy `npm test` runs (attempt 6's
// captured vitest output) — stays near the first-call floor, and the task
// message is on every call, unchanged.
func TestTwentyIterationImplementationRunStaysNearTheFloor(t *testing.T) {
	root := demoAppTree(t)
	vitest, err := os.ReadFile("../../internal/llm/tools/testdata/vitest_npm_test.log") // the one copy, shared with the filter tests
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) adapter.ToolCall {
		return adapter.ToolCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, p)}
	}
	readRange := func(p string, s, e int) adapter.ToolCall {
		return adapter.ToolCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q,"start_line":%d,"end_line":%d}`, p, s, e)}
	}
	edit := func(p string, line int) adapter.ToolCall {
		old := fmt.Sprintf("// %s line %03d of the original file\n", filepath.Base(p), line)
		args, _ := json.Marshal(map[string]string{"path": p, "old_string": old, "new_string": old + "const tilerApiKey = process.env.TILER_API_KEY || '';\n"})
		return adapter.ToolCall{Name: "edit_file", Arguments: string(args)}
	}
	npmTest := adapter.ToolCall{Name: "run_command", Arguments: `{"command":"npm test"}`}
	script := [][]adapter.ToolCall{
		{read("src/config/env.ts"), read("tests/unit/server.test.ts")},
		{readRange("src/server/index.ts", 1, 120)},
		{edit("tests/unit/server.test.ts", 20)},
		{npmTest},
		{edit("src/config/env.ts", 10)},
		{edit("src/server/index.ts", 60)},
		{npmTest},
		{readRange("public/index.html", 820, 880)},
		{edit("public/index.html", 840)},
		{readRange("landing/index.html", 830, 890)},
		{edit("landing/index.html", 855)},
		{adapter.ToolCall{Name: "codex_grep", Arguments: `{"pattern":"no API keys"}`}},
		{readRange("README.md", 1, 60)},
		{edit("README.md", 30)},
		{read(".env.example"), read("docker-compose.yml")},
		{edit(".env.example", 5)},
		{readRange("docs/configuration.md", 1, 60)},
		{edit("docs/configuration.md", 12)},
		{npmTest},
		{adapter.ToolCall{Name: "run_command", Arguments: `{"command":"npm run lint"}`}},
	}
	var turns []adapter.CompletionResponse
	for i, calls := range script {
		for k := range calls {
			calls[k].ID = fmt.Sprintf("call_%d", k) // Ollama restarts ids every turn
		}
		turns = append(turns, adapter.CompletionResponse{Content: fmt.Sprintf("Step %d.", i+1), ToolCalls: calls, FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 60})
	}
	model := &floorRunAdapter{turns: turns}
	ex := tools.NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		switch call.Name {
		case "run_command":
			if strings.Contains(call.Arguments, "lint") {
				return adapter.ToolResult{Content: "\n> demo-app@0.1.0 lint\n> eslint src\n"}, true
			}
			out, _ := tools.ShapeRunOutput("npm test", string(vitest), false)
			return adapter.ToolResult{Content: out}, true
		case "codex_grep":
			return adapter.ToolResult{Content: `{"pattern":"no API keys","total_hits":1,"hits":[{"file":"landing/index.html","line":412,"text":"No API keys, no sign-up."}]}`}, true
		}
		return adapter.ToolResult{}, false // the real read and edit tools on the tree
	})

	req := firstImplementationRequest(t)
	task := req.Messages[1].Content
	floor := tools.EstimatePromptTokens(req)
	scope, _ := taskScope(attempt6PlanTask(t))
	opts := implementationLoopOptions(req.Tools, func() bool { return false }, scope)
	if _, err := tools.RunToolLoopWithOptions(context.Background(), model, req, ex, 60, nil, nil, opts); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(model.reqs) != len(script)+1 {
		t.Fatalf("requests = %d, want %d", len(model.reqs), len(script)+1)
	}
	sum, peak := 0, 0
	for i, r := range model.reqs {
		if r.Messages[1].Role != adapter.RoleUser || r.Messages[1].Content != task {
			t.Fatalf("request %d: the task message is missing or changed", i+1)
		}
		est := tools.EstimatePromptTokens(r)
		sum += est
		peak = max(peak, est)
	}
	mean := float64(sum) / float64(len(model.reqs))
	t.Logf("20-iteration implementation run: floor %d, mean %.0f (%.2fx), peak %d, last %d est. tokens",
		floor, mean, mean/float64(floor), peak, tools.EstimatePromptTokens(model.reqs[len(model.reqs)-1]))
	if mean > 1.8*float64(floor) {
		t.Fatalf("mean estimated input %.0f is %.2fx the floor %d, want <= 1.8x", mean, mean/float64(floor), floor)
	}
	// Every edit landed: the pinned reads gave the model the current text.
	b, _ := os.ReadFile(filepath.Join(root, "landing", "index.html"))
	if !strings.Contains(string(b), "TILER_API_KEY") {
		t.Fatal("landing/index.html edit did not land")
	}
}
