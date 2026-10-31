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

// attempt13bPlanTask is attempt 13b's plan_tasks.message (subtask
// 99a8e484), rewritten onto the demo-app fixture, with the seeds it was
// planned with.
func attempt13bPlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	var task implementationTaskMessage
	b, err := os.ReadFile("testdata/attempt13b/plan_task.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	if len(task.LineSeeds) != 3 {
		t.Fatalf("recorded seeds = %+v", task.LineSeeds)
	}
	return task
}

// R15 T1: attempt 13b's step 3 cites AC-1 and AC-8 ("append `?api_key=<key>`
// to the Tiler tile URLs"), neither of which names an HTML page, so R12's
// path-named rule gave no Target line. The step's files have quoted tile
// URLs in "## Where to edit": the Target falls back to those lines.
func TestAttempt13bStepTargetFallsBackToTheQuotedLines(t *testing.T) {
	task := attempt13bPlanTask(t)
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(task)},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	msg := req.Messages[1].Content
	target := "   Target: modify the quoted literal(s) at public/index.html:840,848 and landing/index.html:855 — not a new layer/block.\n"
	if n := strings.Count(msg, "Target:"); n != 1 {
		t.Fatalf("%d Target lines:\n%s", n, msg)
	}
	at := strings.Index(msg, "3. Modify HTML Pages to Fetch Client Configuration")
	next := at + strings.Index(msg[at:], "\n4. ") + 1
	if at < 0 || !strings.Contains(msg[at:next], target) {
		t.Fatalf("step 3 lacks %q:\n%s", target, msg)
	}
	// The room: step 2's body restates AC-7 with single instead of double
	// quotes, and is no longer repeated.
	if strings.Contains(msg, "{'tilerApiKey'") {
		t.Errorf("step 2's body restating AC-7 is repeated:\n%s", msg)
	}
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-13b task): task %d chars = %d est. tokens", len(msg), floor)
	if len(msg) > taskMessageCeiling {
		t.Errorf("task message %d chars, ceiling %d", len(msg), taskMessageCeiling)
	}
	if floor > implementationFloorCeiling {
		t.Errorf("floor %d > %d", floor, implementationFloorCeiling)
	}
}
