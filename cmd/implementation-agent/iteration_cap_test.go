package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The tool loop's iteration cap ended the pass as a failure ("tool loop
// error: tool loop reached safety limit: 40 iterations") and the attempt
// reported it — five applied edits discarded, the subtask blocked. The cap
// is a stop, not a failure: the pass ends and its work is tested and
// committed, as a stuck pass's is.
func TestIterationCapEndsThePassKeepingItsWork(t *testing.T) {
	model := &queueAdapter{tokens: 100, steps: []step{
		readStep(), writeStep("alpha"), writeStep("done"), readStep(), readStep(), readStep(), readStep(),
	}}
	h := newRepairHarness(t, model, nil, nil)
	h.maxIters = 4 // the model still wants tools when the cap is reached
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped != nil || len(h.sink.published) != 0 || len(h.sink.blocked) != 0 {
		t.Fatalf("stopped=%+v published=%+v blocked=%v, want the pass to end at the cap with its work kept", gen.stopped, h.sink.published, h.sink.blocked)
	}
	if gen.repair == nil || !gen.repair.passed {
		t.Fatalf("repair = %+v, want the pass's work (feature.txt = done) tested and passing", gen.repair)
	}
	out, err := exec.Command("git", "-C", h.f.worktree, "show", h.f.branch+":feature.txt").Output()
	if err != nil || strings.TrimSpace(string(out)) != "done" {
		t.Fatalf("feature.txt on %s = %q (%v), want the pass's last write committed", h.f.branch, out, err)
	}
}
