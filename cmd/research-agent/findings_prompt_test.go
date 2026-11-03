package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// The task message carried the codex sections as raw JSON (11.7k chars in
// the rerun) on every call, so the model never had to query the codex. It
// is one line now: the codex is reached through its tools.
func TestResearchUserMessageCarriesNoCodexDump(t *testing.T) {
	task := testTask("code_trace")
	task.CodexSections = `{"modules":[{"name":"enmime","purpose":"` + strings.Repeat("p", 11000) + `"}]}`
	msg := buildResearchUserMessage(task, "/repo", "")
	if strings.Contains(msg, "## Codex sections") || strings.Contains(msg, `"modules"`) {
		t.Fatalf("message still carries the codex dump (%d chars)", len(msg))
	}
	if !strings.Contains(msg, "Project knowledge: codex_find_module") {
		t.Fatalf("message lacks the pointer to the codex tools:\n%s", msg[len(msg)-300:])
	}
	if !strings.Contains(msg, "## Request (PRD)") {
		t.Fatal("the PRD must still travel once")
	}
}

// The strategy tells the model its reads are ephemeral and its findings
// are its memory, within the strategy's 25-line cap.
func TestResearchStrategyTellsTheModelReadsAreEphemeral(t *testing.T) {
	sys := buildResearchSystemPrompt("code_trace")
	for _, want := range []string{"record_finding", "one turn", "Known findings"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if n := strings.Count(strings.TrimSpace(researchStrategy), "\n") + 1; n > 25 {
		t.Fatalf("strategy is %d lines, cap 25", n)
	}
}

func TestKnownFindingsSectionIsCappedAndQuotesExcerpts(t *testing.T) {
	one := []tools.CitedFinding{{Claim: "Option is the option type", Path: "options.go", StartLine: 12, EndLine: 14, Excerpt: "type Option func(*Parser)\n// more"}}
	got := knownFindingsSection(one)
	want := "\n\n## Known findings (recorded by earlier research on this work item; cite them as they are, or verify with one read)\n" +
		"- Option is the option type (options.go:12-14) — \"type Option func(*Parser) // more\""
	if got != want {
		t.Fatalf("section =\n%q\nwant\n%q", got, want)
	}
	if knownFindingsSection(nil) != "" {
		t.Fatal("no findings must render nothing")
	}
	var many []tools.CitedFinding
	for i := 0; i < 60; i++ {
		many = append(many, tools.CitedFinding{Claim: fmt.Sprintf("finding %02d %s", i, strings.Repeat("c", 120)), Path: "p.go", StartLine: i, EndLine: i, Excerpt: strings.Repeat("e", 200)})
	}
	capped := knownFindingsSection(many)
	if len(capped) > knownFindingsMax {
		t.Fatalf("section is %d chars, cap %d", len(capped), knownFindingsMax)
	}
	if !strings.Contains(capped, "finding 00") {
		t.Fatal("the first (newest, as loaded) findings must be the ones kept")
	}
	// Excerpts are quoted at most 120 chars, on one line.
	if strings.Contains(capped, strings.Repeat("e", 121)) {
		t.Fatal("excerpt not bounded to 120 chars")
	}
}
