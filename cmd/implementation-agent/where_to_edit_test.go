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

// attempt9PlanTask is attempt 9's plan_tasks.message (subtask 8a867abd),
// rewritten onto the demo-app fixture, with the line seeds research-coordinator
// extracts for it (testdata/attempt9/line_seeds.json: the tile-layer and
// createApp lines of testdata/attempt9/demo-app).
func attempt9PlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	var task implementationTaskMessage
	b, err := os.ReadFile("testdata/attempt9/plan_task.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	s, err := os.ReadFile("testdata/attempt9/line_seeds.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(s, &task.LineSeeds); err != nil {
		t.Fatal(err)
	}
	return task
}

// R10 W5: the task message quotes the tile-layer lines research found, so
// the model edits the URL strings instead of inventing a selector.
func TestTaskMessageWhereToEdit(t *testing.T) {
	task := attempt9PlanTask(t)
	msg := buildTaskMessage(task)
	for _, want := range []string{
		"## Where to edit",
		"- public/index.html:836-853\n",
		"    840:     L.tileLayer('https://{s}.basemaps.example.com/dark_nolabels/{z}/{x}/{y}{r}.png', {\n",
		"    848:     const labelsLayer = L.tileLayer('https://{s}.basemaps.example.com/dark_only_labels/",
		"- landing/index.html:855\n    855:     L.tileLayer('https://{s}.basemaps.example.com/dark_all/",
		"- src/server/index.ts:33-60\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("task message lacks %q", want)
		}
	}
	sec := whereToEdit(task.LineSeeds)
	t.Logf("attempt-9 task message: %d chars, Where to edit %d chars", len(msg), len(sec))
	if len(sec) > whereToEditMaxChars {
		t.Fatalf("Where to edit is %d chars", len(sec))
	}
	if len(msg) > taskMessageCeiling {
		t.Fatalf("task message %d chars, ceiling %d", len(msg), taskMessageCeiling)
	}
	// Bounded: many long seeds stay within the cap.
	var many []lineSeed
	for i := 0; i < 20; i++ {
		many = append(many, lineSeed{Path: "a.ts", LineStart: i + 1, LineEnd: i + 1, Snippet: strings.Repeat("x", 400) + "\n2\n3\n4\n5\n6\n7"})
	}
	if n := len(whereToEdit(many)); n > whereToEditMaxChars {
		t.Fatalf("unbounded: %d chars", n)
	}
	// An unverified seed gives its place, never its snippet.
	un := whereToEdit([]lineSeed{{Path: "src/server/index.ts", LineStart: 33, LineEnd: 60, Snippet: "export function createApp(redis) {"}})
	if !strings.Contains(un, "- src/server/index.ts:33-60\n") || strings.Contains(un, "createApp") {
		t.Fatalf("unverified seed quoted:\n%s", un)
	}
	if whereToEdit(nil) != "" {
		t.Fatal("no seeds: no section")
	}
	if got := seedLines(task.LineSeeds); got["public/index.html"] != 840 || got["landing/index.html"] != 855 || got["src/server/index.ts"] != 33 {
		t.Fatalf("seed lines = %v", got)
	}
}

// The handler hands the seeds to the executor's default read window.
func TestHandlerSetsReadSeeds(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(src), "\r\n", "\n"), "executor.SetReadSeeds(seedLines(task.LineSeeds))") {
		t.Fatal("main.go does not set the read seeds")
	}
}

// The first-call floor with attempt 9's task and its seeds (R10: report
// the floor after W5; the ceiling stays 3,500).
func TestImplementationFirstCallFloorWithSeeds(t *testing.T) {
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(attempt9PlanTask(t))},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-9 task with Where to edit): task %d chars = %d est. tokens", len(req.Messages[1].Content), floor)
	if floor > implementationFloorCeiling {
		t.Fatalf("floor %d > %d", floor, implementationFloorCeiling)
	}
}
