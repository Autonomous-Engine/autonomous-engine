package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// R12 X5: attempt 12 opened with 9 parallel read_file calls (every file in
// scope, about 125k chars). The prompt says to read only the file about to
// be edited, around its Where-to-edit lines; independent searches may
// still go in one turn.
func TestPromptAsksForOneFileAtATime(t *testing.T) {
	sys := standardSystemPrompt()
	want := "Read only the file you are about to edit, around the Where-to-edit lines; do not open every in-scope file at once."
	if !strings.Contains(sys, want) {
		t.Fatalf("system prompt lacks %q:\n%s", want, sys)
	}
	if strings.Contains(sys, "Put independent reads") {
		t.Error("the prompt still asks for independent reads in one turn")
	}
	// The floors move by at most 50 estimated tokens (the sentence is
	// about 115 chars).
	req := firstImplementationRequest(t)
	t.Logf("attempt-6 floor with X5: %d est. tokens, system prompt %d chars", tools.EstimatePromptTokens(req), len(sys))
}
