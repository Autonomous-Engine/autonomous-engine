package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

func badFeatureEdit() step {
	return step{calls: call("edit_file", map[string]any{"path": "feature.txt", "old_string": "no such text", "new_string": "done"})}
}

// R10 W3: a loop that ends with LoopStuck before changing anything stops
// the attempt on the structured failure path (loop_stuck), with the stuck
// call as the escalation's failure record — not "no code changes", and
// not a budget burn.
func TestLoopStuckBeforeAnyChangeStopsTheAttempt(t *testing.T) {
	model := &queueAdapter{steps: []step{readStep(), badFeatureEdit(), readStep(), badFeatureEdit(), badFeatureEdit(), readStep()}, tokens: 100}
	h := newRepairHarness(t, model, nil, nil)
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopLoopStuck {
		t.Fatalf("stopped = %+v, want loop_stuck", gen.stopped)
	}
	if model.calls != 5 {
		t.Fatalf("model calls = %d, want 5 (ended at the third identical failure)", model.calls)
	}
	s := h.sink
	if len(s.published) != 1 || s.published[0].Success || !strings.HasPrefix(s.published[0].Reason, "loop_stuck: LoopStuck: edit_file on feature.txt failed 3 times") {
		t.Fatalf("published = %+v", s.published)
	}
	if len(s.blocked) != 1 || len(s.prompts) != 1 {
		t.Fatalf("blocked %d, prompts %d", len(s.blocked), len(s.prompts))
	}
	rec := stuckRecords([]tools.LoopStuck{*mustStuck(t, gen)})[0]
	if q := s.prompts[0].Question; !strings.Contains(q, failures.Short(rec.Fingerprint)) || !strings.Contains(q, "feature.txt") {
		t.Fatalf("escalation does not list the stuck call:\n%s", q)
	}
	if ev := s.published[0].Evidence; ev == nil || ev.Checks[0].SkipReason == "" || !strings.HasPrefix(ev.Checks[0].SkipReason, "loop stuck: ") {
		t.Fatalf("evidence = %+v", ev)
	}
}

func mustStuck(t *testing.T, gen generationResult) *tools.LoopStuck {
	t.Helper()
	if len(gen.stopped.stuck) == 0 {
		t.Fatal("no LoopStuck recorded")
	}
	return &gen.stopped.stuck[len(gen.stopped.stuck)-1]
}

// R10 review: a LoopStuck in the no-edits retry is recorded, and with
// nothing changed the attempt stops as loop_stuck, not "no code changes".
func TestLoopStuckInNoEditsRetryStopsTheAttempt(t *testing.T) {
	model := &queueAdapter{steps: []step{readStep(), textStep("Done."), badFeatureEdit(), badFeatureEdit(), badFeatureEdit(), readStep()}, tokens: 100}
	h := newRepairHarness(t, model, nil, nil)
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopLoopStuck || len(gen.stopped.stuck) != 1 {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	if model.calls != 5 {
		t.Fatalf("model calls = %d, want 5", model.calls)
	}
}
