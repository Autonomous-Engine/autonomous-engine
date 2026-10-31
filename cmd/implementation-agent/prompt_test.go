package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

const systemPromptCeiling = 4000 // chars, T2 (attempt 6: 9,786)

func standardSystemPrompt() string {
	return assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")
}

// T2: the system prompt is short, framework-free and does not contradict
// itself.
func TestImplementationSystemPromptIsShortAndConsistent(t *testing.T) {
	for _, mode := range []models.OperatingMode{models.OperatingModeConstrained, models.OperatingModeStandard, models.OperatingModeAdvanced} {
		sys := assembleImplementationSystemPrompt(mode, "", "npm test", "", "")
		t.Logf("%s system prompt: %d chars", mode, len(sys))
		if len(sys) > systemPromptCeiling {
			t.Errorf("%s: system prompt is %d chars, want <= %d", mode, len(sys), systemPromptCeiling)
		}
		low := strings.ToLower(sys)
		for _, banned := range []string{"next.js", "react", "use client", "one tool_call at a time", "first message must",
			"project codex", "multi-repo", "mandatory", "do not open with"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s: system prompt contains %q", mode, banned)
			}
		}
		for _, want := range []string{"several tool calls", "edit_file", "codex_verify_imports", "Test command: npm test"} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s: system prompt lacks %q", mode, want)
			}
		}
	}
	// With the no-runner note and the bounded grounding it still fits.
	long := strings.Repeat("- app/feature: renders the page (risk: low)\n", 100)
	sys := assembleImplementationSystemPrompt(models.OperatingModeStandard, long, "", noTestRunnerNote, "")
	if len(sys) > systemPromptCeiling+maxImplementationGroundingChars {
		t.Errorf("with grounding and the no-runner note: %d chars", len(sys))
	}
	if strings.Contains(sys, "Test command:") || !strings.Contains(sys, "No test runner") {
		t.Errorf("no-runner prompt:\n%s", sys)
	}
}

// The tier preamble used to say "before calling tools, reply with a short
// bullet plan" while the implementation prompt said "your FIRST message
// MUST be a tool_call". The preamble now asks for the plan in the same
// reply as the first tool calls.
func TestTierPreambleAsksForThePlanWithTheFirstToolCalls(t *testing.T) {
	pre := strategy.PromptScaffolding(models.OperatingModeStandard).PromptPreamble()
	if strings.Contains(pre, "Before calling tools") {
		t.Errorf("preamble still asks for a plan before any tool call:\n%s", pre)
	}
	if !strings.Contains(pre, "same reply") {
		t.Errorf("preamble does not put the plan in the reply with the first tool calls:\n%s", pre)
	}
}

// The prompt names only tools the edit phase offers.
func TestImplementationPromptNamesOnlyOfferedTools(t *testing.T) {
	offered := toolNames(editTools())
	sys := standardSystemPrompt()
	all := append(tools.SafeImplementationTools(), tools.ImplementationTools()...)
	for _, tl := range all {
		named := regexp.MustCompile(`(^|[^a-z_])` + tl.Name + `([^a-z_]|$)`)
		if !offered[tl.Name] && named.MatchString(sys) {
			t.Errorf("system prompt names %s, which the edit phase does not offer", tl.Name)
		}
	}
}

// The handler builds its system prompt with the measured assembly.
func TestHandlerUsesTheMeasuredPromptAssembly(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(s, "systemPrompt := assembleImplementationSystemPrompt(") {
		t.Error("main.go does not use assembleImplementationSystemPrompt")
	}
	for _, banned := range []string{"strategy.ImplementStrategy", "PromptPreamble()", "buildScopeSection(task)"} {
		if strings.Contains(s, banned) {
			t.Errorf("main.go still uses %s", banned)
		}
	}
}
