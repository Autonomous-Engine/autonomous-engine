package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R11 S3: a continuation pass's edits go through the pre-commit checks and
// the bounded repair loop before they are committed. The harness is the
// repair loop's (repairRunner fails unless feature.txt says "done"); the
// task plans feature.txt and extra.txt, the main loop finishes feature.txt
// and says it still has work, and the continuation adds extra.txt but
// breaks feature.txt — attempt 11's continuation adding a failing test.

func fileStep(path, content string) step {
	return step{calls: call("write_file", map[string]any{"path": path, "content": content})}
}
func readPathStep(path string) step {
	return step{calls: call("read_file", map[string]any{"path": path})}
}

func continuationHarness(t *testing.T, contSteps ...step) (*repairHarness, generationResult) {
	t.Helper()
	steps := []step{readStep(), writeStep("done"), textStep("Implemented the first part; I still need the rest.")}
	steps = append(steps, contSteps...)
	model := &queueAdapter{tokens: 100, steps: steps}
	h := newRepairHarness(t, model, nil, nil)
	h.planned = []string{"feature.txt", "extra.txt"}
	h.clonePath = t.TempDir() // not the worktree: it stays on the agent branch
	return h, h.attempt(t, nil, h.sink)
}

func commitsSinceBase(t *testing.T, h *repairHarness) []string {
	t.Helper()
	out := strings.TrimSpace(gitOutput(t, h.f.worktree, "log", "--format=%s", h.f.baseSHA+"..HEAD"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// The continuation's failing edit gets one repair, the checks pass, and
// the pass is committed.
func TestContinuationFailingTestIsRepairedBeforeCommit(t *testing.T) {
	h, gen := continuationHarness(t,
		readPathStep("extra.txt"), fileStep("extra.txt", "extra\n"), readStep(), writeStep("alpha"), textStep("Added extra."),
		// repair 1
		readStep(), writeStep("done"), textStep("Fixed."),
	)
	if gen.stopped != nil || len(gen.continuationFailures) != 0 {
		t.Fatalf("gen = %+v", gen)
	}
	commits := commitsSinceBase(t, h)
	if len(commits) != 2 || !strings.Contains(commits[0], "(cont 1)") {
		t.Fatalf("commits = %q, want the main commit and the continuation's", commits)
	}
	if got := strings.TrimSpace(gitOutput(t, h.f.worktree, "show", "HEAD:feature.txt")); got != "done" {
		t.Fatalf("committed feature.txt = %q: a red continuation was committed", got)
	}
	if got := strings.TrimSpace(gitOutput(t, h.f.worktree, "show", "HEAD:extra.txt")); got != "extra" {
		t.Fatalf("committed extra.txt = %q", got)
	}
	var st repairState
	h.rec.dmu.Lock()
	err := json.Unmarshal(h.rec.detail["continuation_1_repair"], &st)
	h.rec.dmu.Unlock()
	if err != nil || st.Repairs != 1 || st.Runs != 2 || st.StopReason != stopPassed {
		t.Fatalf("continuation repair state = %+v (%v)", st, err)
	}
	// The main loop's own state is not overwritten.
	if main := h.rec.repair(t); main.Runs != 1 || main.StopReason != stopPassed {
		t.Fatalf("main repair state = %+v", main)
	}
	if len(h.sink.published) != 0 {
		t.Fatalf("generation reported %+v", h.sink.published)
	}
}

// The same failure after the repair: a structured failure is recorded, the
// pass's edits are reverted, nothing of it is committed, and generation
// goes on to finalize the green tree it committed before.
func TestContinuationStillFailingIsRevertedNotCommitted(t *testing.T) {
	h, gen := continuationHarness(t,
		readPathStep("extra.txt"), fileStep("extra.txt", "extra\n"), readStep(), writeStep("alpha"), textStep("Added extra."),
		// repair 1 rewrites the same wrong value
		readStep(), writeStep("alpha"), textStep("Fixed."),
	)
	if gen.stopped != nil {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	if len(gen.continuationFailures) != 1 {
		t.Fatalf("continuation failures = %+v", gen.continuationFailures)
	}
	f := gen.continuationFailures[0]
	if f.Pass != 1 || f.Stop != stopRepeatedFailure || f.Repairs != 1 || len(f.Fingerprints) != 1 ||
		strings.Join(f.Reverted, ",") != "extra.txt,feature.txt" {
		t.Fatalf("failure = %+v", f)
	}
	if commits := commitsSinceBase(t, h); len(commits) != 1 || strings.Contains(commits[0], "(cont") {
		t.Fatalf("commits = %q, want only the main commit", commits)
	}
	if st := strings.TrimSpace(gitOutput(t, h.f.worktree, "status", "--porcelain")); st != "" {
		t.Fatalf("worktree not clean after the revert:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(h.f.worktree, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("extra.txt survived the revert (%v)", err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.f.worktree, "feature.txt")); strings.TrimSpace(string(b)) != "done" {
		t.Fatalf("feature.txt = %q after the revert", b)
	}
	// Recorded: a note event with the failure, the run detail, the summary.
	var noted bool
	for _, e := range h.rec.eventsOf("note") {
		noted = noted || strings.HasPrefix(e.Note, "continuation 1 reverted: pre-commit checks repeated_failure")
	}
	h.rec.dmu.Lock()
	_, inDetail := h.rec.detail["continuation_1_reverted"]
	h.rec.dmu.Unlock()
	if !noted || !inDetail {
		t.Fatalf("failure not recorded (note %v, detail %v)", noted, inDetail)
	}
	if !strings.Contains(gen.summary, "Continuation pass 1 was not committed") {
		t.Fatalf("summary = %q", gen.summary)
	}
	if len(h.sink.published) != 0 {
		t.Fatalf("generation reported %+v", h.sink.published)
	}
}
