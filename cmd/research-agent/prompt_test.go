package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// floorBefore is the first-call floor measured at 778666e for the same
// inputs as TestFirstCallFloor: 20,807 chars of schemas (ResearchTools),
// a 9,292-char system prompt carrying the PRD and the CodexFirst tutorial,
// and a 4,783-char user message carrying the PRD again.
const floorBefore = 8720

const floorCeiling = 3200 // estimated tokens, R1 acceptance

// testPRD is a 4.6k-char PRD with a unique marker on its first line.
func testPRD() string {
	var b strings.Builder
	b.WriteString("# TILES-MARKER Map layer toggle\n\n## Overview\n")
	for b.Len() < 4600 {
		b.WriteString("The landing page map must let a visitor toggle the station layer without a reload. ")
	}
	return b.String()[:4600]
}

func testTask(researchType string) researchTaskMessage {
	return researchTaskMessage{
		TaskID:          "task-1",
		WorkID:          "00000000-0000-0000-0000-000000000001",
		ProductID:       "00000000-0000-0000-0000-000000000002",
		ResearchType:    researchType,
		StartingContext: testPRD(),
	}
}

// fullGrounding is a worst-case codex section: a product with enough
// modules and patterns to fill maxCodexGroundingChars.
func fullGrounding() string {
	var mods []*models.CodexModule
	for i := 0; i < 60; i++ {
		mods = append(mods, &models.CodexModule{Name: fmt.Sprintf("app/feature-%02d", i), Purpose: "renders the feature page and calls its API route", RiskLevel: "medium"})
	}
	g, err := buildCodexGrounding(context.Background(), &fakeCodex{modules: map[string][]*models.CodexModule{"main": mods}}, uuid.New(), "main")
	if err != nil {
		panic(err)
	}
	return g
}

// firstRequest builds the first research request with the handler's own
// assembly functions, the Standard tier preamble and the given codex
// grounding.
func firstRequest(researchType string, lspReady bool) adapter.CompletionRequest {
	return firstRequestWith(researchType, lspReady, "")
}

func firstRequestWith(researchType string, lspReady bool, grounding string) adapter.CompletionRequest {
	system := assembleResearchSystemPrompt(researchType, grounding, models.OperatingModeStandard)
	user := buildResearchUserMessage(testTask(researchType), "/repo-cache/p/sample-app", "")
	return adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: system},
			{Role: adapter.RoleUser, Content: user},
		},
		Tools: researchToolSet(researchType, lspReady),
	}
}

// The floor is measured with a full (capped) codex grounding section: the
// worst case once B1 lets grounding through.
func TestFirstCallFloor(t *testing.T) {
	grounding := fullGrounding()
	if len(grounding) < maxCodexGroundingChars-200 {
		t.Fatalf("test grounding is %d chars, want it near the %d cap", len(grounding), maxCodexGroundingChars)
	}
	for _, g := range []struct {
		name, text string
	}{{"no grounding", ""}, {"full grounding", grounding}} {
		req := firstRequestWith("code_trace", false, g.text)
		schema, _ := json.Marshal(req.Tools)
		floor := tools.EstimatePromptTokens(req)
		t.Logf("first-call floor (code_trace, 4.6k PRD, %s): schemas %d + system %d + user %d chars = %d est. tokens (before: %d)",
			g.name, len(schema), len(req.Messages[0].Content), len(req.Messages[1].Content), floor, floorBefore)
		if floor > floorCeiling {
			t.Fatalf("first-call floor (%s) %d est. tokens, want <= %d", g.name, floor, floorCeiling)
		}
	}
}

// The handler builds its system prompt with the same function the floor
// test measures.
func TestHandlerUsesTheMeasuredAssembly(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	for _, want := range []string{
		"systemPrompt := assembleResearchSystemPrompt(task.ResearchType, codexContext, operatingMode)",
		"userMessage := buildResearchUserMessage(task, primaryPath,",
		"Tools:       researchToolSet(task.ResearchType, executor.LSPReady(msgCtx)),",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	if strings.Contains(s, "PromptPreamble()") {
		t.Error("main.go assembles the preamble itself instead of using assembleResearchSystemPrompt")
	}
}

func TestPRDIsSentOnceInTheUserMessage(t *testing.T) {
	for _, typ := range []string{"code_trace", "history_analysis", "pattern_match", "impact_assessment", "bug_archaeology", "other"} {
		req := firstRequest(typ, false)
		if strings.Contains(req.Messages[0].Content, "TILES-MARKER") {
			t.Errorf("%s: system prompt carries the PRD", typ)
		}
		if n := strings.Count(req.Messages[1].Content, "TILES-MARKER"); n != 1 {
			t.Errorf("%s: user message carries the PRD %d times, want 1", typ, n)
		}
	}
}

func TestSystemPromptIsShortAndExampleFree(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	if n := len(strings.Split(strings.TrimSpace(norm(researchStrategy)), "\n")); n > 25 {
		t.Errorf("strategy is %d lines, want <= 25", n)
	}
	for _, typ := range []string{"code_trace", "history_analysis", "pattern_match", "impact_assessment", "bug_archaeology", "other"} {
		g := norm(researchTypeGuidance(typ))
		if n := len(strings.Split(strings.TrimSpace(g), "\n")); n > 8 {
			t.Errorf("%s guidance is %d lines, want <= 8", typ, n)
		}
		sys := buildResearchSystemPrompt(typ)
		for _, banned := range []string{"Prisma", "prisma", "locations", "Worked example", "Coverage rule", "STOP AS SOON AS you have 3+"} {
			if strings.Contains(sys, banned) {
				t.Errorf("%s system prompt still contains %q", typ, banned)
			}
		}
		for _, want := range []string{"acceptance criteria", "evidence ledger"} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s system prompt lacks %q", typ, want)
			}
		}
	}
}

// The prompt names only tools the task is offered.
func TestTypeGuidanceNamesOnlyOfferedTools(t *testing.T) {
	for _, typ := range []string{"code_trace", "history_analysis", "pattern_match", "impact_assessment", "bug_archaeology", "other"} {
		offered := map[string]bool{}
		for _, tl := range researchToolSet(typ, false) {
			offered[tl.Name] = true
		}
		sys := buildResearchSystemPrompt(typ)
		for _, tl := range tools.ResearchTools() {
			named := regexp.MustCompile(`(^|[^a-z_])` + tl.Name + `([^a-z_]|$)`)
			if !offered[tl.Name] && named.MatchString(sys) {
				t.Errorf("%s prompt names %s, which it is not offered", typ, tl.Name)
			}
		}
	}
}

// replayAdapter returns scripted tool-call turns and records the estimated
// input of every request.
type replayAdapter struct {
	turns []adapter.CompletionResponse
	n     int
	ests  []int
}

func (a *replayAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.ests = append(a.ests, tools.EstimatePromptTokens(req))
	if a.n < len(a.turns) {
		t := a.turns[a.n]
		a.n++
		return &t, nil
	}
	return &adapter.CompletionResponse{Content: "Findings: the toggle lives in public/index.html:840.", FinishReason: "stop", OutputTokens: 60}, nil
}
func (a *replayAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *replayAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 32768, SupportsFunctions: true}
}
func (a *replayAdapter) HealthCheck(context.Context) error { return nil }
func (a *replayAdapter) Name() string                      { return "replay/test" }

// realisticResult mirrors the result sizes measured on item 9f163d1f:
// module lookups up to ~3.8k, file reads 2–3k, grep/tree results small.
func realisticResult(call adapter.ToolCall) adapter.ToolResult {
	var a map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &a)
	pad := func(n int) string { return strings.Repeat("y", n) }
	var v any
	switch call.Name {
	case "codex_find_module":
		v = map[string]any{"modules": []map[string]any{{"name": fmt.Sprint(a["name_or_pattern"]), "purpose": pad(2400)}}, "count": 1}
	case "codex_read_file":
		v = map[string]any{"path": a["path"], "start_line": 1, "end_line": 80, "total_lines": 900, "content": "<!doctype html>\n" + pad(2800)}
	case "codex_grep":
		v = map[string]any{"pattern": a["pattern"], "total_hits": 3, "hits": []map[string]any{{"file": "public/index.html", "line": 840, "text": pad(150)}, {"file": "landing/index.html", "line": 855, "text": pad(150)}, {"file": "server.js", "line": 12, "text": pad(100)}}}
	case "codex_list_tree":
		v = map[string]any{"path": a["path"], "tree": "./\n  public/\n    index.html\n  landing/\n    index.html\n  server.js\n" + pad(600)}
	case "codex_similar_code":
		v = map[string]any{"results": []map[string]any{{"file": "public/map.js", "function": "toggleLayer"}, {"file": "public/index.html"}}, "count": 2}
	default:
		v = map[string]any{"module": a["module_name"], "dependents": []string{"public/index.html", "server.js"}}
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return adapter.ToolResult{Content: string(b)}
}

func TestTwentyIterationRunStaysNearTheFloor(t *testing.T) {
	names := []string{"codex_find_module", "codex_grep", "codex_read_file", "codex_list_tree", "codex_similar_code", "codex_dependents_of"}
	var turns []adapter.CompletionResponse
	for i := 0; i < 20; i++ {
		name := names[i%len(names)]
		var args string
		switch name {
		case "codex_find_module":
			args = fmt.Sprintf(`{"name_or_pattern":"map%d"}`, i)
		case "codex_grep":
			args = fmt.Sprintf(`{"pattern":"toggle%d"}`, i)
		case "codex_read_file":
			args = fmt.Sprintf(`{"path":"public/f%d.html","start_line":1,"end_line":80}`, i)
		case "codex_list_tree":
			args = fmt.Sprintf(`{"path":"dir%d"}`, i)
		case "codex_similar_code":
			args = fmt.Sprintf(`{"description":"station layer %d"}`, i)
		default:
			args = fmt.Sprintf(`{"module_name":"mod%d"}`, i)
		}
		turns = append(turns, adapter.CompletionResponse{
			Content:      fmt.Sprintf("Step %d.", i+1),
			ToolCalls:    []adapter.ToolCall{{ID: fmt.Sprintf("c%d", i+1), Name: name, Arguments: args}},
			FinishReason: "tool_calls", InputTokens: 1, OutputTokens: 40,
		})
	}
	model := &replayAdapter{turns: turns}
	ex := tools.NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		return realisticResult(call), true
	})
	req := firstRequest("code_trace", false)
	floor := tools.EstimatePromptTokens(req)
	opts := researchLoopOptions("code_trace", func() bool { return false })
	if _, err := tools.RunToolLoopWithOptions(context.Background(), model, req, ex, 40, nil, nil, opts); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if len(model.ests) != 21 {
		t.Fatalf("calls = %d, want 21", len(model.ests))
	}
	sum := 0
	for _, e := range model.ests {
		sum += e
	}
	mean := float64(sum) / float64(len(model.ests))
	t.Logf("20-iteration run: floor %d, mean %.0f (%.2fx), last %d est. tokens", floor, mean, mean/float64(floor), model.ests[len(model.ests)-1])
	if mean > 1.6*float64(floor) {
		t.Fatalf("mean estimated input %.0f tokens is %.2fx the floor %d, want <= 1.6x", mean, mean/float64(floor), floor)
	}
}

// ─── B1: codex grounding resolves the branch and never drops errors ──────

type fakeCodex struct {
	modules map[string][]*models.CodexModule
	asked   []string
}

func (f *fakeCodex) GetModulesForProductBranch(_ context.Context, _ uuid.UUID, branch string) ([]*models.CodexModule, error) {
	f.asked = append(f.asked, branch)
	if branch == "" {
		return nil, errors.New("codex: empty branch")
	}
	m, ok := f.modules[branch]
	if !ok {
		return nil, fmt.Errorf("codex: no snapshot for branch %q", branch)
	}
	return m, nil
}
func (f *fakeCodex) GetPatternsForProductBranch(_ context.Context, _ uuid.UUID, branch string) ([]*models.CodexPattern, error) {
	if branch == "" {
		return nil, errors.New("codex: empty branch")
	}
	return []*models.CodexPattern{{Name: "fetch-wrapper", Category: "http", Rationale: "one place for retries"}}, nil
}
func (f *fakeCodex) GetAntiPatternsForProductBranch(_ context.Context, _ uuid.UUID, branch string) ([]*models.CodexAntiPattern, error) {
	if branch == "" {
		return nil, errors.New("codex: empty branch")
	}
	return nil, nil
}

func TestCodexGroundingUsesMainWhenTheItemHasNoBranch(t *testing.T) {
	cx := &fakeCodex{modules: map[string][]*models.CodexModule{
		"main": {{Name: "public/index.html", Purpose: "landing page with the station map", RiskLevel: "medium"}},
	}}
	noDefault := func(context.Context) (string, error) { return "", errors.New("no default branch configured") }
	branch, branchErr := resolveCodexBranch(context.Background(), "", noDefault)
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if branchErr == nil || !strings.Contains(branchErr.Error(), "no default branch configured") {
		t.Fatalf("default-branch error = %v, want it returned for logging", branchErr)
	}
	grounding, err := buildCodexGrounding(context.Background(), cx, uuid.New(), branch)
	if err != nil {
		t.Fatalf("grounding: %v", err)
	}
	system := buildResearchSystemPrompt("code_trace") + grounding
	if !strings.Contains(system, "public/index.html: landing page with the station map") {
		t.Fatalf("modules for main are missing from the prompt:\n%s", system)
	}
	if !strings.Contains(system, "fetch-wrapper") {
		t.Fatalf("patterns for main are missing from the prompt")
	}
	for _, b := range cx.asked {
		if b == "" {
			t.Fatal("codex was asked for the empty branch")
		}
	}
}

func TestCodexBranchPrefersTheItemThenTheProductDefault(t *testing.T) {
	def := func(context.Context) (string, error) { return "master", nil }
	if b, err := resolveCodexBranch(context.Background(), "feature/x", def); b != "feature/x" || err != nil {
		t.Fatalf("branch = %q, %v; want the item's branch", b, err)
	}
	if b, err := resolveCodexBranch(context.Background(), "", def); b != "master" || err != nil {
		t.Fatalf("branch = %q, %v; want the product default", b, err)
	}
}

func TestCodexGroundingReportsLookupFailures(t *testing.T) {
	cx := &fakeCodex{modules: map[string][]*models.CodexModule{}}
	grounding, err := buildCodexGrounding(context.Background(), cx, uuid.New(), "develop")
	if err == nil || !strings.Contains(err.Error(), "develop") {
		t.Fatalf("err = %v, want the failed lookup reported", err)
	}
	// What did load (patterns) is still used.
	if !strings.Contains(grounding, "fetch-wrapper") {
		t.Fatalf("grounding = %q, want the patterns that did load", grounding)
	}
}

func TestCodexGroundingIsBounded(t *testing.T) {
	var mods []*models.CodexModule
	for i := 0; i < 200; i++ {
		mods = append(mods, &models.CodexModule{Name: fmt.Sprintf("src/module-%03d", i), Purpose: strings.Repeat("does things ", 6), RiskLevel: "low"})
	}
	cx := &fakeCodex{modules: map[string][]*models.CodexModule{"main": mods}}
	grounding, err := buildCodexGrounding(context.Background(), cx, uuid.New(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(grounding) > maxCodexGroundingChars+200 {
		t.Fatalf("grounding is %d chars, want about <= %d", len(grounding), maxCodexGroundingChars)
	}
	if !strings.Contains(grounding, "more modules") {
		t.Fatalf("a truncated module list must say so")
	}
}

// Source scan: the handler must not discard codex errors (B1).
func TestHandlerDoesNotDiscardCodexErrors(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(s, "branch, branchErr := resolveCodexBranch(") || !strings.Contains(s, "errors.Join(branchErr, gErr)") {
		t.Error("main.go does not log the default-branch lookup error with the grounding error")
	}
	for _, bad := range []string{"modules, _ := cx.", "patterns, _ := cx.", "antiPatterns, _ := cx.", "tools.ResearchTools()", "branch, _ := resolveCodexBranch("} {
		if strings.Contains(s, bad) {
			t.Errorf("main.go still contains %q", bad)
		}
	}
}
