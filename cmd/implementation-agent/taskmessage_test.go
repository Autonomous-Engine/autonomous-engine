package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// attempt6PlanTask is attempt 6's plan_tasks.message (subtask 35af01de,
// work item e69051fa), rewritten onto the demo-app fixture.
func attempt6PlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt6_task.json")
	if err != nil {
		t.Fatal(err)
	}
	var task implementationTaskMessage
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

const taskMessageCeiling = 5000 // chars, T3 (attempt 6: 12,105)

// T3: the task is sent once, de-duplicated: steps once with their files and
// criteria, criteria once, no restating brief, the files in scope.
func TestTaskMessageIsDeduplicated(t *testing.T) {
	task := attempt6PlanTask(t)
	msg := buildTaskMessage(task)
	t.Logf("task message: %d chars (attempt 6: 12,105)\n%s", len(msg), msg)
	if len(msg) > taskMessageCeiling {
		t.Errorf("task message is %d chars, want <= %d", len(msg), taskMessageCeiling)
	}
	steps := []string{
		"Configure Tiler API Key in Environment", "Add /api/client-config Endpoint", "Modify HTML Pages to Fetch Client Config",
		"Add User Feedback for Missing API Key", "Log Warning for Missing API Key at Server Startup",
		"Document Tiler API Key Requirement", "Fix 'no API keys' Claim in landing/index.html", "Add Unit Test for /api/client-config",
	}
	stepsSection := msg[strings.Index(msg, "## Steps"):strings.Index(msg, "## Acceptance criteria")]
	for i, s := range steps {
		if n := strings.Count(stepsSection, string(rune('1'+i))+". "+s); n != 1 {
			t.Errorf("step %d title %q appears %d times", i+1, s, n)
		}
		if !strings.Contains(msg, string(rune('1'+i))+". "+s) {
			t.Errorf("step %d is not numbered: %q", i+1, s)
		}
	}
	for _, c := range task.Criteria {
		if strings.Count(msg, "- "+c.ID+": ") != 1 {
			t.Errorf("criterion %s not listed exactly once", c.ID)
		}
		if n := strings.Count(msg, strings.TrimSpace(c.Text)); n != 1 {
			t.Errorf("criterion %s text appears %d times", c.ID, n)
		}
	}
	scope, _ := taskScope(task)
	if len(scope) != 9 {
		t.Fatalf("scope = %v", scope)
	}
	files := msg[strings.Index(msg, "## Files in scope"):]
	for _, f := range scope {
		if !strings.Contains(files, f) {
			t.Errorf("Files in scope lacks %s", f)
		}
	}
	for _, banned := range []string{"Source: user", "Deliverable", "Technical Brief", "Executive Summary", "After reviewing",
		"Grounding note", "Next.js", "File evidence", "cited c", "Repository clone path", "Multi-repo"} {
		if strings.Contains(msg, banned) {
			t.Errorf("task message contains %q", banned)
		}
	}
	if strings.Count(msg, "`npm test`") != 1 || strings.Count(msg, "`npm run lint`") != 1 {
		t.Errorf("checks are not listed once each")
	}
}

// A task without steps (legacy) keeps its description and the brief, with
// the model preamble and the grounding notes removed.
func TestTaskMessageForALegacyTask(t *testing.T) {
	task := implementationTaskMessage{
		Title:          "Fix login redirect",
		Description:    "Redirect to /home after login.\n\n## Grounding note — orphan files\n\nfor Next.js projects: …",
		TechnicalBrief: "After reviewing the brief, no changes are needed:\n\n## Summary\nThe redirect lives in auth.ts.",
		FilesToChange:  []string{"src/auth.ts"},
	}
	msg := buildTaskMessage(task)
	for _, want := range []string{"Task: Fix login redirect", "Redirect to /home after login.", "## Summary\nThe redirect lives in auth.ts.", "## Files in scope\nsrc/auth.ts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("legacy message lacks %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{"Grounding note", "After reviewing"} {
		if strings.Contains(msg, banned) {
			t.Errorf("legacy message contains %q", banned)
		}
	}
}

// The handler sends the measured task message.
func TestHandlerSendsTheTaskMessage(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(s, "taskInstructions := buildTaskMessage(task)") {
		t.Error("main.go does not build the task message with buildTaskMessage")
	}
}

// implementationFloorBefore is attempt 6's measured first call (input_tokens
// of inference_logs call 1): 27 schemas (23,401 chars), a 9,786-char system
// prompt and a 12,105-char task message.
const implementationFloorBefore = 10254

const implementationFloorCeiling = 3500 // estimated tokens, R6 acceptance

// firstImplementationRequest is the main loop's first request for the
// attempt-6 task: the handler's own assembly, the Standard tier, the edit
// phase without LSP tools (not ready at the first call), no grounding.
func firstImplementationRequest(t *testing.T) adapter.CompletionRequest {
	return adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(attempt6PlanTask(t))},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
}

func TestImplementationFirstCallFloor(t *testing.T) {
	req := firstImplementationRequest(t)
	schema, _ := json.Marshal(req.Tools)
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-6 task): schemas %d + system %d + task %d chars = %d est. tokens (before: %d measured)",
		len(schema), len(req.Messages[0].Content), len(req.Messages[1].Content), floor, implementationFloorBefore)
	if floor > implementationFloorCeiling {
		t.Fatalf("first-call floor %d est. tokens, want <= %d", floor, implementationFloorCeiling)
	}
}
