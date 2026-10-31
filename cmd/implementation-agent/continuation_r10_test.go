package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// R10 W4: the remaining files are listed code, tests, configuration, docs.
func TestContinuationOrdersTestsBeforeDocs(t *testing.T) {
	missing := []string{"docs/configuration.md", "README.md", "package.json", "docker-compose.yml", "tests/unit/server.test.ts", ".env.example", "src/server/index.ts"}
	got := continuationOrder(missing)
	want := []string{"src/server/index.ts", "tests/unit/server.test.ts", "package.json", "docker-compose.yml", ".env.example", "docs/configuration.md", "README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	p := continuationPrompt(map[string]bool{"src/config/env.ts": true}, missing, nil)
	if strings.Index(p, "tests/unit/server.test.ts") > strings.Index(p, "docs/configuration.md") {
		t.Fatalf("prompt lists docs before the test:\n%s", p)
	}
}

// R10 W4: a continuation after a stuck pass gets the stuck call's reason
// (with the edit result's hints) and is told not to repeat it; a missing
// file the loop got stuck on always continues.
func TestContinuationInheritsTheStuckFailure(t *testing.T) {
	stuck := []tools.LoopStuck{{Tool: "edit_file", Path: "tests/unit/server.test.ts", Attempts: 3,
		Reason: "REJECTED: edit would duplicate a block. new_string lines 2-9 re-type file lines 22-29, which already follow old_string:\n    22:   let app: ReturnType<typeof createApp>['app'];"}}
	p := continuationPrompt(map[string]bool{"src/config/env.ts": true}, []string{"tests/unit/server.test.ts"}, stuck)
	for _, want := range []string{"edit_file on tests/unit/server.test.ts (3 attempts)", "re-type file lines 22-29", "let app: ReturnType<typeof createApp>['app'];", "Do not repeat those calls"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	// Outside the steps, at half the scope done, with a finished-sounding
	// summary, a stuck file still continues.
	scope := []string{"a.ts", "notes.md"}
	if _, cont := continuationDecision(scope, nil, map[string]bool{"a.ts": true}, "All done.", 0); cont {
		t.Fatal("precondition: without the stuck file the old heuristic stops")
	}
	if _, cont := continuationDecision(scope, nil, map[string]bool{"a.ts": true}, "All done.", 0, "notes.md"); !cont {
		t.Fatal("a stuck file did not continue")
	}
	if !sameStuck(stuck, tools.LoopStuck{Tool: "edit_file", Path: "tests/unit/server.test.ts"}) || sameStuck(stuck, tools.LoopStuck{Tool: "edit_file", Path: "README.md"}) {
		t.Fatal("sameStuck")
	}
}

func notesStep(name string, args map[string]any) step { return step{calls: call(name, args)} }

// R10 W4 end to end: pass 1 changes feature.txt and gets stuck on
// notes.md; its work is tested and committed. The continuation gets the
// reason; repeating the same failing edit ends it at once, and the attempt
// stops on the structured failure path with both stuck failures.
func TestContinuationRepeatingTheStuckEditEndsTheAttempt(t *testing.T) {
	readNotes := notesStep("read_file", map[string]any{"path": "notes.md"})
	badNotes := func() step {
		return notesStep("edit_file", map[string]any{"path": "notes.md", "old_string": "## Config", "new_string": "## Config\nTILER_API_KEY"})
	}
	model := &queueAdapter{steps: []step{
		readStep(), writeStep("done"), readNotes, badNotes(), badNotes(), badNotes(), // pass 1: stuck on notes.md
		readNotes, badNotes(), readNotes, // continuation: the same edit again
	}, tokens: 100}
	h := newRepairHarness(t, model, nil, nil)
	h.planned = []string{"feature.txt", "notes.md"}
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopLoopStuck || len(gen.stopped.stuck) != 2 {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	if model.calls != 8 {
		t.Fatalf("model calls = %d, want 8 (the continuation ended at its first repeat)", model.calls)
	}
	var contPrompt string
	for _, p := range model.prompts {
		if strings.Contains(p, "## Continuation") || strings.Contains(p, "You have completed part of this task") {
			contPrompt = p
		}
	}
	if !strings.Contains(contPrompt, "edit_file on notes.md (3 attempts)") || !strings.Contains(contPrompt, "Do not repeat those calls") {
		t.Fatalf("continuation prompt:\n%s", contPrompt)
	}
	if got := strings.TrimSpace(gitOutput(t, h.f.worktree, "show", h.f.branch+":feature.txt")); got != "done" {
		t.Fatalf("committed feature.txt = %q", got)
	}
	if len(h.sink.published) != 1 || !strings.HasPrefix(h.sink.published[0].Reason, "loop_stuck: ") {
		t.Fatalf("published = %+v", h.sink.published)
	}
}
