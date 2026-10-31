package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// attempt6Spec is a live run's subtask (attempt 6): files_to_change named only
// src/server/index.ts while the 8-step task_spec cited env.ts, both HTML
// pages, README and docs.
func attempt6Task() implementationTaskMessage {
	repo := plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: plan.RefStat}
	return implementationTaskMessage{
		FilesToChange: []string{"src/server/index.ts"},
		TaskSpec: &plan.TaskSpec{Files: []plan.FileRef{
			{Path: "src/config/env.ts", Evidence: repo},
			{Path: "src/server/index.ts", Evidence: repo},
			{Path: "public/index.html", Evidence: repo},
			{Path: "landing/index.html", Evidence: repo},
			{Path: "README.md", Evidence: repo},
			{Path: "docs/configuration.md", Evidence: repo},
			{Path: "tests/unit/server.test.ts", Evidence: repo},
			{Path: "src/missing.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: "grounding: not found in the pinned snapshot"}},
		}},
	}
}

// TestContinuationConsidersStepFiles: after the first pass touched three
// files and the model's final message reads as finished, the implementer
// must still continue because steps' files are untouched — attempt 6
// logged "all planned files covered — no continuation needed" instead.
func TestContinuationConsidersStepFiles(t *testing.T) {
	task := attempt6Task()
	scope, steps := taskScope(task)
	changed := map[string]bool{"src/server/index.ts": true, "src/config/env.ts": true, "tests/unit/server.test.ts": true, "node_modules": true}
	transcriptFinal := "I have added the Tiler API key to the configuration and exposed it through /api/client-config. The implementation is complete."

	missing, cont := continuationDecision(scope, steps, changed, transcriptFinal, 0)
	want := []string{"public/index.html", "landing/index.html", "README.md", "docs/configuration.md"}
	if !cont || !reflect.DeepEqual(missing, want) {
		t.Fatalf("continuationDecision = %v, %v; want continue with %v", missing, cont, want)
	}

	// Once every step file is touched, no continuation.
	for _, f := range want {
		changed[f] = true
	}
	if missing, cont := continuationDecision(scope, steps, changed, transcriptFinal, 1); cont || len(missing) != 0 {
		t.Fatalf("all step files touched: continuationDecision = %v, %v; want stop", missing, cont)
	}
}

// A legacy task (no task_spec) keeps the old rule: when at least half the
// planned files changed on the first pass and the model sounds finished,
// stop; when it sounds unfinished, continue.
func TestContinuationLegacyHeuristic(t *testing.T) {
	task := implementationTaskMessage{FilesToChange: []string{"a.go", "b.go", "c.go"}}
	scope, steps := taskScope(task)
	changed := map[string]bool{"a.go": true, "b.go": true}
	if _, cont := continuationDecision(scope, steps, changed, "Done.", 0); cont {
		t.Fatal("legacy: 2/3 changed and finished-sounding summary must not continue")
	}
	if missing, cont := continuationDecision(scope, steps, changed, "Next step: I'll update c.go", 0); !cont || !reflect.DeepEqual(missing, []string{"c.go"}) {
		t.Fatalf("legacy: unfinished-sounding summary = %v, %v; want continue with [c.go]", missing, cont)
	}
	if _, cont := continuationDecision(scope, steps, map[string]bool{"a.go": true}, "Done.", 0); !cont {
		t.Fatal("legacy: 1/3 changed must continue")
	}
}

func TestTaskScopeIsTheUnion(t *testing.T) {
	scope, steps := taskScope(attempt6Task())
	if len(scope) != 7 || scope[0] != "src/server/index.ts" {
		t.Fatalf("scope = %v, want files_to_change first then the 6 other step files", scope)
	}
	if len(steps) != 7 {
		t.Fatalf("step files = %v, want the 7 grounded step files", steps)
	}
}

// R6 M3: a planned file may need no change (it was read for context); the
// continuation says so instead of pushing an edit.
func TestContinuationPromptAllowsNoChange(t *testing.T) {
	p := continuationPrompt(map[string]bool{"src/server/index.ts": true}, []string{"README.md", "public/index.html"}, nil)
	for _, want := range []string{"src/server/index.ts", "  - README.md\n", "  - public/index.html\n",
		"If a listed file needs no change, say so and do not edit it."} {
		if !strings.Contains(p, want) {
			t.Errorf("continuation prompt lacks %q:\n%s", want, p)
		}
	}
	// The pass keeps the task: what README.md needs is only there.
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: "system"},
		{Role: adapter.RoleUser, Content: "Task: TASK-MARKER"},
		{Role: adapter.RoleAssistant, Content: "done"},
	}, MaxTokens: 8192}
	c := continuationRequest(req, p)
	if len(c.Messages) != 2 || c.Messages[0].Content != "system" || !strings.HasPrefix(c.Messages[1].Content, "Task: TASK-MARKER\n\n## Continuation\n") || !strings.HasSuffix(c.Messages[1].Content, p) || c.MaxTokens != 8192 {
		t.Errorf("continuation request = %+v", c)
	}
}
