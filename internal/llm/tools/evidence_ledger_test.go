package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// scriptAdapter replays tool-call turns, then a final answer, and keeps a
// copy of every request it was sent.
type scriptAdapter struct {
	mu    sync.Mutex
	turns []adapter.CompletionResponse
	n     int
	reqs  []adapter.CompletionRequest
}

func (a *scriptAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := req
	cp.Messages = append([]adapter.ChatMessage(nil), req.Messages...)
	cp.Tools = append([]adapter.Tool(nil), req.Tools...)
	a.reqs = append(a.reqs, cp)
	if a.n < len(a.turns) {
		t := a.turns[a.n]
		a.n++
		return &t, nil
	}
	return &adapter.CompletionResponse{Content: "Final answer: the flow is in a.ts:12.", FinishReason: "stop", InputTokens: 1, OutputTokens: 50}, nil
}
func (a *scriptAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *scriptAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 200000, SupportsFunctions: true}
}
func (a *scriptAdapter) HealthCheck(context.Context) error { return nil }
func (a *scriptAdapter) Name() string                      { return "script/test" }

type scriptedCall struct{ name, args string }

func turnsFor(calls []scriptedCall, content func(i int) string) []adapter.CompletionResponse {
	out := make([]adapter.CompletionResponse, 0, len(calls))
	for i, c := range calls {
		out = append(out, adapter.CompletionResponse{
			Content:      content(i),
			ToolCalls:    []adapter.ToolCall{{ID: fmt.Sprintf("call-%d", i+1), Name: c.name, Arguments: c.args}},
			FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40,
		})
	}
	return out
}

// sizedResult builds a result shaped like the real tool's output, padded to
// about size chars.
func sizedResult(name, args string, size int) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(args), &a)
	filler := func(n int) string {
		if n < 0 {
			n = 0
		}
		return strings.Repeat("x", n)
	}
	var v any
	switch name {
	case "codex_find_module":
		v = map[string]any{"modules": []map[string]any{{"name": "mod-" + fmt.Sprint(a["name_or_pattern"]), "purpose": filler(size - 120)}, {"name": "mod-two", "purpose": "p"}}, "count": 2, "query": a["name_or_pattern"]}
	case "codex_grep":
		v = map[string]any{"pattern": a["pattern"], "total_hits": 4, "hits": []map[string]any{
			{"file": "src/a.ts", "line": 3, "text": filler(size - 200)}, {"file": "src/b.ts", "line": 9, "text": "t"},
			{"file": "src/c.ts", "line": 27, "text": "t"}, {"file": "src/d.ts", "line": 40, "text": "t"}}}
	case "codex_read_file":
		v = map[string]any{"path": a["path"], "start_line": 1, "end_line": 120, "total_lines": 120,
			"content": "// header comment\nexport function handle(req) {\n" + filler(size-150)}
	case "codex_list_tree":
		v = map[string]any{"path": a["path"], "tree": "./\n  src/\n    a.ts\n  public/\n    index.html\n  package.json\n" + "    " + filler(size-120) + "\n"}
	case "codex_similar_code":
		v = map[string]any{"results": []map[string]any{{"file": "src/a.ts", "function": filler(size - 150)}, {"file": "src/e.ts"}}, "count": 2}
	default:
		return filler(size)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func sizedOverride(size int) func(context.Context, adapter.ToolCall) (adapter.ToolResult, bool) {
	return func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		return adapter.ToolResult{Content: sizedResult(call.Name, call.Arguments, size)}, true
	}
}

var fifteenCalls = []scriptedCall{
	{"codex_find_module", `{"name_or_pattern":"auth"}`},
	{"codex_grep", `{"pattern":"login"}`},
	{"codex_read_file", `{"path":"src/a.ts"}`},
	{"codex_list_tree", `{"path":"."}`},
	{"codex_similar_code", `{"description":"session handling"}`},
	{"codex_find_module", `{"name_or_pattern": "auth"}`}, // repeat of 1 (spacing differs)
	{"codex_grep", `{"pattern":"logout"}`},
	{"codex_read_file", `{"path":"src/b.ts"}`},
	{"codex_list_tree", `{"path":"src"}`},
	{"codex_similar_code", `{"description":"token refresh"}`},
	{"codex_grep", `{"pattern":"login"}`}, // repeat of 2
	{"codex_read_file", `{"path":"src/c.ts"}`},
	{"codex_find_module", `{"name_or_pattern":"user"}`},
	{"codex_grep", `{"pattern":"signup"}`},
	{"codex_read_file", `{"path":"src/d.ts"}`},
}

func baseRequest() adapter.CompletionRequest {
	return adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: "You are a research agent."},
			{Role: adapter.RoleUser, Content: "Research task: code_trace\n" + strings.Repeat("PRD text. ", 400)},
		},
		Tools:      ResearchToolsFor("code_trace", ResearchToolOptions{}),
		ToolChoice: "auto",
	}
}

func ledgerOf(msgs []adapter.ChatMessage) (string, int) {
	for i, m := range msgs {
		if isLedgerMessage(m) {
			return m.Content, i
		}
	}
	return "", -1
}

func ledgerLines(ledger string) []string {
	var out []string
	for _, ln := range strings.Split(ledger, "\n") {
		if strings.HasPrefix(ln, "- ") {
			out = append(out, ln)
		}
	}
	return out
}

// assertProtocol checks every tool message answers a tool call of an
// earlier assistant message, and every tool call is answered.
func assertProtocol(t *testing.T, msgs []adapter.ChatMessage) {
	t.Helper()
	open := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case adapter.RoleAssistant:
			for id := range open {
				t.Fatalf("tool call %s has no result before the next assistant turn", id)
			}
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case adapter.RoleTool:
			if !open[m.ToolCallID] {
				t.Fatalf("tool message %s answers no open tool call", m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		}
	}
}

func TestEvidenceLedgerBoundsThePromptOverFifteenIterations(t *testing.T) {
	const resultSize = 3000
	content := func(i int) string {
		if i == 0 {
			return strings.Repeat("Plan: find the auth module, then trace login. ", 20) // 900+ chars
		}
		return fmt.Sprintf("Checking step %d.", i+1)
	}
	model := &scriptAdapter{turns: turnsFor(fifteenCalls, content)}
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(sizedOverride(resultSize))

	resp, err := RunToolLoopWithOptions(context.Background(), model, baseRequest(), ex, 40, nil, nil, LoopOptions{EvidenceLedger: true})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if resp == nil || !strings.HasPrefix(resp.Content, "Final answer") {
		t.Fatalf("resp = %+v", resp)
	}
	if len(model.reqs) != 16 {
		t.Fatalf("LLM calls = %d, want 16", len(model.reqs))
	}

	base := EstimatePromptTokens(model.reqs[0])
	const ceiling = 2500 // tokens above the first call, for any later call
	maxSeen := 0
	for i, r := range model.reqs {
		assertProtocol(t, r.Messages)
		est := EstimatePromptTokens(r)
		if est > maxSeen {
			maxSeen = est
		}
		if est > base+ceiling {
			t.Errorf("call %d: estimated prompt %d tokens, ceiling %d (first call %d + %d)", i+1, est, base+ceiling, base, ceiling)
		}
	}
	t.Logf("first call %d tokens, largest %d tokens (ceiling %d)", base, maxSeen, base+ceiling)

	last := model.reqs[15].Messages
	ledger, at := ledgerOf(last)
	if at != 2 {
		t.Fatalf("ledger at index %d, want 2 (right after the first user message)", at)
	}
	// A user-role message: adapters fold system messages into the leading
	// system block, where a changing ledger would defeat prefix caching of
	// system + tools + PRD.
	if last[at].Role != adapter.RoleUser {
		t.Fatalf("ledger role = %s, want user", last[at].Role)
	}
	for i, r := range model.reqs {
		for _, m := range r.Messages {
			if m.Role == adapter.RoleSystem && strings.Contains(m.Content, ledgerHeader) {
				t.Fatalf("call %d: ledger sent as a system message", i+1)
			}
		}
		// The prefix before the ledger (system + first user) never changes.
		if r.Messages[0].Content != model.reqs[0].Messages[0].Content || r.Messages[1].Content != model.reqs[0].Messages[1].Content {
			t.Fatalf("call %d: the system/first-user prefix changed", i+1)
		}
	}
	t.Logf("ledger before call 16:\n%s", ledger)
	lines := ledgerLines(ledger)
	// Iterations 1–13 are compacted; two of them repeat an earlier call.
	if len(lines) != 11 {
		t.Fatalf("ledger has %d lines, want 11 (one per distinct call):\n%s", len(lines), ledger)
	}
	repeated := 0
	for _, ln := range lines {
		if strings.HasSuffix(ln, "(repeated)") {
			repeated++
		}
		if len([]rune(ln)) > 2+80+160+40 {
			t.Errorf("ledger line too long (%d): %s", len([]rune(ln)), ln)
		}
	}
	if repeated != 2 {
		t.Fatalf("repeated lines = %d, want 2:\n%s", repeated, ledger)
	}
	for _, want := range []string{
		`- codex_find_module("name_or_pattern":"auth") -> 2 modules: mod-auth, mod-two (repeated)`,
		`- codex_grep("pattern":"login") -> 4 hits: src/a.ts:3, src/b.ts:9, src/c.ts:27 (repeated)`,
		`- codex_read_file("path":"src/a.ts") -> src/a.ts L1-120 of 120 lines; export function handle(req) {`,
		`- codex_list_tree("path":".") -> 3 entries: src/, public/, package.json`,
		`- codex_similar_code("description":"session handling") -> 2 matches: src/a.ts, src/e.ts`,
	} {
		if !strings.Contains(ledger, want) {
			t.Errorf("ledger lacks %q:\n%s", want, ledger)
		}
	}

	// The last two iterations are present in full.
	var tools []adapter.ChatMessage
	var calls []adapter.ChatMessage
	for _, m := range last {
		if m.Role == adapter.RoleTool {
			tools = append(tools, m)
		}
		if m.Role == adapter.RoleAssistant && len(m.ToolCalls) > 0 {
			calls = append(calls, m)
		}
	}
	if len(tools) != 2 || len(calls) != 2 {
		t.Fatalf("full iterations kept: %d tool results, %d tool-call turns; want 2 and 2", len(tools), len(calls))
	}
	if tools[0].ToolCallID != "call-14" || tools[1].ToolCallID != "call-15" {
		t.Fatalf("kept results %s, %s; want call-14, call-15", tools[0].ToolCallID, tools[1].ToolCallID)
	}
	if tools[1].Content != sizedResult("codex_read_file", `{"path":"src/d.ts"}`, resultSize) {
		t.Fatal("the latest tool result is not intact")
	}
	// Older assistant turns are trimmed to 300 chars.
	for _, m := range last {
		if m.Role == adapter.RoleAssistant && len(m.ToolCalls) == 0 && len([]rune(m.Content)) > 300 {
			t.Errorf("old assistant turn not trimmed: %d chars", len([]rune(m.Content)))
		}
	}
}

// Ollama numbers tool calls call_0, call_1 … afresh in every response, so
// the same ID recurs across iterations. The ledger must record the calls
// whose results it removed, not the latest call with that ID.
func TestEvidenceLedgerHandlesToolCallIDsRepeatedAcrossIterations(t *testing.T) {
	var turns []adapter.CompletionResponse
	for i := 1; i <= 5; i++ {
		turns = append(turns, adapter.CompletionResponse{
			ToolCalls:    []adapter.ToolCall{{ID: "call_0", Name: "codex_grep", Arguments: fmt.Sprintf(`{"pattern":"q%d"}`, i)}},
			FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40,
		})
	}
	model := &scriptAdapter{turns: turns}
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		var a map[string]string
		_ = json.Unmarshal([]byte(call.Arguments), &a)
		b, _ := json.Marshal(map[string]any{"pattern": a["pattern"], "total_hits": 1,
			"hits": []map[string]any{{"file": "src/" + a["pattern"] + ".ts", "line": 7, "text": "t"}}})
		return adapter.ToolResult{Content: string(b)}, true
	})
	if _, err := RunToolLoopWithOptions(context.Background(), model, baseRequest(), ex, 40, nil, nil, LoopOptions{EvidenceLedger: true, KeepRecent: 2}); err != nil {
		t.Fatal(err)
	}
	last := model.reqs[len(model.reqs)-1].Messages
	ledger, _ := ledgerOf(last)
	lines := ledgerLines(ledger)
	want := []string{
		`- codex_grep("pattern":"q1") -> 1 hits: src/q1.ts:7`,
		`- codex_grep("pattern":"q2") -> 1 hits: src/q2.ts:7`,
		`- codex_grep("pattern":"q3") -> 1 hits: src/q3.ts:7`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ledger lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// The two kept iterations are q4 and q5, in full.
	var kept []string
	for _, m := range last {
		if m.Role == adapter.RoleTool {
			kept = append(kept, m.Content)
		}
	}
	if len(kept) != 2 || !strings.Contains(kept[0], "q4") || !strings.Contains(kept[1], "q5") {
		t.Fatalf("kept results = %v, want q4 and q5", kept)
	}
}

func TestWithoutTheLedgerTheTranscriptKeepsEveryResult(t *testing.T) {
	model := &scriptAdapter{turns: turnsFor(fifteenCalls[:5], func(int) string { return "" })}
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(sizedOverride(500))
	if _, err := RunToolLoop(context.Background(), model, baseRequest(), ex, 40, nil, nil); err != nil {
		t.Fatal(err)
	}
	last := model.reqs[len(model.reqs)-1].Messages
	n := 0
	for _, m := range last {
		if m.Role == adapter.RoleTool {
			n++
		}
		if isLedgerMessage(m) {
			t.Fatal("ledger present without the option")
		}
	}
	if n != 5 {
		t.Fatalf("tool results = %d, want all 5", n)
	}
}

func TestDeadToolIsDroppedAfterTwoNotReadyErrors(t *testing.T) {
	calls := []scriptedCall{
		{"codex_workspace_symbols", `{"query":"Location"}`},
		{"codex_find_module", `{"name_or_pattern":"auth"}`},
		{"codex_workspace_symbols", `{"query":"Map"}`},
		{"codex_grep", `{"pattern":"x"}`},
		{"codex_read_file", `{"path":"a.ts"}`},
		{"codex_list_tree", `{"path":"."}`},
	}
	run := func(guard bool) *scriptAdapter {
		model := &scriptAdapter{turns: turnsFor(calls, func(int) string { return "" })}
		ex := NewExecutor(t.TempDir())
		ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
			if call.Name == "codex_workspace_symbols" {
				return adapter.ToolResult{Content: "codex_workspace_symbols: lsp: typescript workspace not ready: context deadline exceeded — fall back to codex_similar_code or codex_grep.", IsError: true}, true
			}
			return adapter.ToolResult{Content: sizedResult(call.Name, call.Arguments, 200)}, true
		})
		req := baseRequest()
		req.Tools = ResearchToolsFor("code_trace", ResearchToolOptions{LSPReady: true})
		if _, err := RunToolLoopWithOptions(context.Background(), model, req, ex, 40, nil, nil, LoopOptions{EvidenceLedger: true, DeadToolGuard: guard}); err != nil {
			t.Fatal(err)
		}
		return model
	}
	offered := func(r adapter.CompletionRequest, name string) bool {
		for _, tl := range r.Tools {
			if tl.Name == name {
				return true
			}
		}
		return false
	}

	m := run(true)
	// Calls 1–3 are made with the tool offered; after its second failure
	// (call 3's result) it is gone.
	for i := 0; i < 3; i++ {
		if !offered(m.reqs[i], "codex_workspace_symbols") {
			t.Fatalf("call %d: tool dropped too early", i+1)
		}
	}
	for i := 3; i < len(m.reqs); i++ {
		if offered(m.reqs[i], "codex_workspace_symbols") {
			t.Fatalf("call %d still offers the dead tool", i+1)
		}
	}
	ledger, _ := ledgerOf(m.reqs[len(m.reqs)-1].Messages)
	if !strings.Contains(ledger, "codex_workspace_symbols removed") {
		t.Fatalf("ledger does not say the tool was dropped:\n%s", ledger)
	}

	// Without the guard the tool stays offered.
	m = run(false)
	if !offered(m.reqs[len(m.reqs)-1], "codex_workspace_symbols") {
		t.Fatal("tool dropped without DeadToolGuard")
	}
}

// Only consecutive dead-class failures drop a tool; a success in between
// resets the count.
func TestDeadToolGuardCountsConsecutiveFailuresOnly(t *testing.T) {
	calls := []scriptedCall{
		{"codex_similar_code", `{"description":"a"}`}, // fails
		{"codex_grep", `{"pattern":"x"}`},
		{"codex_similar_code", `{"description":"b"}`}, // succeeds: resets
		{"codex_list_tree", `{"path":"."}`},
		{"codex_similar_code", `{"description":"c"}`}, // fails (1 in a row)
		{"codex_read_file", `{"path":"a.ts"}`},
	}
	model := &scriptAdapter{turns: turnsFor(calls, func(int) string { return "" })}
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		if call.Name == "codex_similar_code" && !strings.Contains(call.Arguments, `"b"`) {
			return adapter.ToolResult{Content: "codex_similar_code unavailable: vector store is not configured.", IsError: true}, true
		}
		return adapter.ToolResult{Content: sizedResult(call.Name, call.Arguments, 200)}, true
	})
	if _, err := RunToolLoopWithOptions(context.Background(), model, baseRequest(), ex, 40, nil, nil, LoopOptions{EvidenceLedger: true, DeadToolGuard: true}); err != nil {
		t.Fatal(err)
	}
	for i, r := range model.reqs {
		found := false
		for _, tl := range r.Tools {
			if tl.Name == "codex_similar_code" {
				found = true
			}
		}
		if !found {
			t.Fatalf("call %d: codex_similar_code dropped after non-consecutive failures", i+1)
		}
	}
}

func TestTransientUnavailableIsNotDead(t *testing.T) {
	for _, s := range []string{
		"codex_similar_code: qdrant: 503 Service Unavailable",
		"embedding provider temporarily unavailable, retry later",
		"HTTP 503: upstream unavailable",
	} {
		if isDeadToolError(s) {
			t.Errorf("%q classified as a dead tool", s)
		}
	}
	for _, s := range []string{
		"codex_workspace_symbols: lsp: typescript workspace not ready: context deadline exceeded",
		"codex_similar_code unavailable: vector store is not configured.",
	} {
		if !isDeadToolError(s) {
			t.Errorf("%q not classified as a dead tool", s)
		}
	}
}

func TestRefreshToolsReplacesTheOfferedList(t *testing.T) {
	model := &scriptAdapter{turns: turnsFor(fifteenCalls[:4], func(int) string { return "" })}
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(sizedOverride(100))
	calls := 0
	refresh := func() []adapter.Tool {
		calls++
		return ResearchToolsFor("code_trace", ResearchToolOptions{LSPReady: calls >= 3})
	}
	if _, err := RunToolLoopWithOptions(context.Background(), model, baseRequest(), ex, 40, nil, nil, LoopOptions{RefreshTools: refresh}); err != nil {
		t.Fatal(err)
	}
	has := func(r adapter.CompletionRequest) bool {
		for _, tl := range r.Tools {
			if tl.Name == "codex_workspace_symbols" {
				return true
			}
		}
		return false
	}
	if has(model.reqs[0]) || has(model.reqs[1]) || !has(model.reqs[2]) {
		t.Fatalf("LSP tools offered on calls: %v %v %v, want false false true", has(model.reqs[0]), has(model.reqs[1]), has(model.reqs[2]))
	}
}

func TestSummaries(t *testing.T) {
	cases := []struct {
		tool, content string
		isErr         bool
		want          string
	}{
		{"codex_find_module", `{"modules":[],"count":0,"query":"server routes"}`, false, "0 results"},
		{"codex_grep", `{"pattern":"index\\.html","total_hits":0,"hits":[]}`, false, "0 results"},
		{"codex_read_file", `{"path":"README.md","start_line":1,"end_line":40,"total_lines":40,"content":"\n# Radr View\ntext"}`, false, "README.md L1-40 of 40 lines; # Radr View"},
		{"codex_workspace_symbols", "codex_workspace_symbols: lsp: typescript workspace not ready: context deadline exceeded", true, "error: not ready"},
		{"codex_similar_code", "codex_similar_code unavailable: vector store is not configured.", true, "error: unavailable"},
		{"codex_read_file", "codex_read_file: open x: no such file or directory", true, "error: not found"},
		{"codex_dependents_of", `{"module":"x","dependents":[],"error":"module not found in codex"}`, false, "module not found"},
		{"codex_dependents_of", `{"module":"x","dependents":["a","b"]}`, false, "2: a, b"},
	}
	for _, c := range cases {
		if got := summarizeToolResult(c.tool, c.content, c.isErr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.tool, got, c.want)
		}
	}
	long := summarizeToolResult("codex_find_module", sizedResult("codex_find_module", `{"name_or_pattern":"`+strings.Repeat("m", 300)+`"}`, 500), false)
	if len([]rune(long)) > 160 {
		t.Errorf("summary is %d chars, want <= 160", len([]rune(long)))
	}
	line := ledgerLine("codex_grep", `{"pattern":"`+strings.Repeat("a", 200)+`"}`, `{"hits":[]}`, false)
	if args := line[strings.Index(line, "(")+1 : strings.LastIndex(line, ") ->")]; len([]rune(args)) > 80 {
		t.Errorf("args rendered at %d chars, want <= 80", len([]rune(args)))
	}
}
