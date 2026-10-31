package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// R8 O4: the repair prompt names, per orphan, the paths in its text and the
// tasks whose files cover them.

const attempt8Hint = "AC-6 mentions landing/index.html, which tasks 3 and 4 change; add AC-6 to one of their criterion_ids"

// Attempt 8: a repair prompt for the attempt-8 plan carries the hint under
// AC-6's violation. (R9 P1: planWithRepair re-homes AC-6 before any
// re-prompt, so the live flow no longer asks; the hint still serves an
// orphan the planner cannot re-home, and is checked here on the live plan.)
func TestAttempt8_RepairPromptNamesTheCandidates(t *testing.T) {
	brief, dec := attempt8Fixture(t)
	folded, folds := foldBrief(uuid.Nil, brief.Criteria)
	tasks := mustTasks(t, []byte(dec))
	normaliseDerivedCriteria(tasks, folded, folds)
	b := capturedBuilder(brief.Criteria)(tasks)
	vs := b.validate()
	repair := repairPrompt(vs, b.raw, repairHints(b, vs))
	if !strings.Contains(repair, attempt8Hint) {
		t.Fatalf("repair prompt lacks %q:\n%s", attempt8Hint, repair)
	}
}

func TestRepairPromptHints(t *testing.T) {
	brief := orphanBrief("A", "B",
		"The \"no sign-up required\" claim in `web/index.html` is corrected.",
		"`src/server.ts` logs a warning",
		"`docs/setup.md` explains the key",
		"The notice reads politely")
	steps := []decomposedTask{
		orphanStep("Pages", []string{"AC-1"}, "web/index.html", "web/app.js"),
		orphanStep("Server", []string{"AC-2"}, "src/server.ts"),
	}
	b := capturedBuilder(brief)(copySteps(steps))
	vs := b.validate()
	hints := orphanRepairHints(b, vs)
	for id, want := range map[string]string{
		"AC-3": "AC-3 mentions web/index.html, which task 1 changes; add AC-3 to its criterion_ids",
		"AC-4": "AC-4 mentions src/server.ts, which task 2 changes; add AC-4 to its criterion_ids",
		"AC-5": "AC-5 mentions docs/setup.md, which no task changes; add AC-5 to the criterion_ids of the task that changes it, and list it in that task's files",
	} {
		if hints[id] != want {
			t.Errorf("hint %s = %q, want %q", id, hints[id], want)
		}
	}
	if _, ok := hints["AC-6"]; ok {
		t.Errorf("AC-6 names no path and must have no hint: %q", hints["AC-6"])
	}

	p := repairPrompt(vs, steps, repairHints(b, vs))
	// The hint follows its own violation line.
	i := strings.Index(p, `AC-3 ("The \"no sign-up required\" claim`)
	j := strings.Index(p, hints["AC-3"])
	k := strings.Index(p, "AC-4 (")
	if i < 0 || j < i || k < j {
		t.Fatalf("hint not under its violation (%d, %d, %d):\n%s", i, j, k, p)
	}
	if strings.Count(p, hints["AC-3"]) != 1 {
		t.Errorf("hint repeated:\n%s", p)
	}
}
