package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// attempt11PlanTask is attempt 11's plan_tasks.message (subtask d6f5b872,
// rewritten onto the demo-app fixture; it carried no line_seeds) with the
// seeds the coordinator extracts for it (testdata/attempt11/line_seeds.json:
// the tile-layer and createApp lines of testdata/attempt9/demo-app).
func attempt11PlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	var task implementationTaskMessage
	b, err := os.ReadFile("testdata/attempt11/plan_task.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	if len(task.LineSeeds) != 0 {
		t.Fatal("the recorded task already has line seeds")
	}
	s, err := os.ReadFile("testdata/attempt11/line_seeds.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(s, &task.LineSeeds); err != nil {
		t.Fatal(err)
	}
	return task
}

// R11 S1: attempt 11's task message has "## Where to edit" quoting the
// tile layers, and the first default read of public/index.html is centred
// on line 840 (attempt 11 read lines 1-401, the CSS).
func TestAttempt11WhereToEditAndCentredRead(t *testing.T) {
	task := attempt11PlanTask(t)
	msg := buildTaskMessage(task)
	for _, want := range []string{
		"## Where to edit",
		"- public/index.html:836-853\n",
		"    840:     L.tileLayer('https://{s}.basemaps.example.com/dark_nolabels/{z}/{x}/{y}{r}.png', {\n",
		"    848:     const labelsLayer = L.tileLayer('https://{s}.basemaps.example.com/dark_only_labels/",
		"- landing/index.html:855\n    855:     L.tileLayer('https://{s}.basemaps.example.com/dark_all/",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("task message lacks %q", want)
		}
	}
	t.Logf("attempt-11 task message: %d chars, Where to edit %d chars", len(msg), len(whereToEdit(task.LineSeeds)))
	if len(msg) > taskMessageCeiling {
		t.Fatalf("task message %d chars, ceiling %d", len(msg), taskMessageCeiling)
	}
	seeds := seedLines(task.LineSeeds)
	if seeds["public/index.html"] != 840 || seeds["landing/index.html"] != 855 {
		t.Fatalf("seed lines = %v", seeds)
	}
	ex := tools.NewExecutor(copyAttempt9Tree(t))
	ex.SetReadSeeds(seeds)
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "r1", Name: "read_file", Arguments: `{"path":"public/index.html"}`})
	// R12 X1: the window is bounded to 4,800 chars around line 840 (it was
	// 640-1039, about 15k chars).
	if r.IsError || !strings.Contains(r.Content, "opened around the Where-to-edit line 840") ||
		!strings.Contains(r.Content, "L.tileLayer('https://{s}.basemaps.example.com/dark_nolabels/") ||
		!strings.Contains(r.Content, "dark_only_labels") || len(r.Content) > 6300 {
		t.Fatalf("first read of public/index.html is not centred on 840: %.300s … %s", r.Content, r.Content[max(0, len(r.Content)-200):])
	}
}

// The first-call floor with attempt 11's task, its seeds and the two
// insertion tools (R11: report the floor; the ceiling stays 3,500).
func TestImplementationFirstCallFloorAttempt11(t *testing.T) {
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(attempt11PlanTask(t))},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-11 task with Where to edit): task %d chars = %d est. tokens", len(req.Messages[1].Content), floor)
	if floor > implementationFloorCeiling {
		t.Fatalf("floor %d > %d", floor, implementationFloorCeiling)
	}
}
