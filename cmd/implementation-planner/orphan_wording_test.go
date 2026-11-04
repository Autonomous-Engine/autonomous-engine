package main

import (
	"strings"
	"testing"
)

// Live (ciblame item 0f565594): AC-3 "Rows are sorted chronologically by
// date" names no repository path, and the model left it out of both
// planning rounds, so the planner recorded it as an unowned gap and the
// verifier escalated it to a person — although the step "Sort the day
// rows" was plainly the step for it. A path-less orphan whose wording one
// step's title or description shares is re-homed to that step: the
// verifier judges it against that step's evidence as usual, and an unmet
// criterion is a fix round, not an escalation.
func TestAPathlessOrphanIsRehomedToTheStepThatSharesItsWording(t *testing.T) {
	brief := orphanBrief("The `--by day` flag groups commits by date", "The grouping label is the UTC date", "Rows are sorted chronologically by date")
	steps := []decomposedTask{
		orphanStep("Add the ByDay grouping", []string{"AC-1"}, "report.go"),
		orphanStep("Sort the day rows", []string{"AC-2"}, "report.go"),
	}
	steps[1].Description = "Sort the rows of the day grouping by date, newest first."
	out, _ := planTwice(t, brief, steps)
	if out.rejected() {
		t.Fatalf("plan rejected: %+v", out.violations)
	}
	b := out.build
	if len(b.unowned) != 0 {
		t.Fatalf("unowned = %v, want none: AC-3 shares its wording with step 2", b.unowned)
	}
	if len(b.rehomed) != 1 || b.rehomed[0].CriterionID != "AC-3" || b.rehomed[0].TaskIndex != 2 {
		t.Fatalf("rehomed = %+v, want AC-3 on step 2", b.rehomed)
	}
	r := b.rehomed[0]
	if len(r.Files) != 0 || strings.Join(r.Words, " ") != "rows sorted date" {
		t.Errorf("rehomed record = %+v, want no files and the shared words rows, sorted, date", r)
	}
	if !containsString(b.raw[1].CriterionIDs, "AC-3") || containsString(b.raw[0].CriterionIDs, "AC-3") {
		t.Errorf("AC-3 cited by step 1 = %t, step 2 = %t; want step 2 only", containsString(b.raw[0].CriterionIDs, "AC-3"), containsString(b.raw[1].CriterionIDs, "AC-3"))
	}

	attachNonCodeCriteria(b)
	var found bool
	for _, p := range b.planned {
		if len(p.msg.NonCodeCriteria) != 0 {
			t.Errorf("non_code_criteria = %+v, want none", p.msg.NonCodeCriteria)
		}
		if strings.Contains(p.msg.Description, "## Re-homed acceptance criteria") {
			found = true
			if !strings.Contains(p.msg.Description, `AC-3 ("Rows are sorted chronologically by date") was owned by no step after planning; the planner added it to step "Sort the day rows" because that step's wording matches it (rows, sorted, date)`) {
				t.Errorf("re-homed section does not say why:\n%s", p.msg.Description)
			}
		}
	}
	if !found {
		t.Error("no task message carries the re-homed section")
	}
}

// Two steps that share the orphan's wording equally are a coin flip the
// planner does not take: the criterion stays an unowned gap, and the gap
// line says why it is unowned.
func TestAPathlessOrphanTwoStepsShareEquallyStaysAGap(t *testing.T) {
	brief := orphanBrief("A", "B", "Rows are sorted by date")
	steps := []decomposedTask{
		orphanStep("Sort rows", []string{"AC-1"}, "a.go"),
		orphanStep("Sort other rows", []string{"AC-2"}, "b.go"),
	}
	out, _ := planTwice(t, brief, steps)
	if out.rejected() {
		t.Fatalf("plan rejected: %+v", out.violations)
	}
	b := out.build
	if len(b.rehomed) != 0 || strings.Join(b.unowned, ",") != "AC-3" {
		t.Fatalf("rehomed=%+v unowned=%v, want AC-3 a gap", b.rehomed, b.unowned)
	}
	want := "criterion AC-3 is unowned after planning (its text names no repository path, and steps 1 and 2 share its wording equally): Rows are sorted by date; verify manually"
	var got string
	for _, v := range out.violations {
		if v.CriterionID == "AC-3" {
			got = v.Message
		}
	}
	if got != want {
		t.Errorf("gap line = %q\n   want %q", got, want)
	}
	attachNonCodeCriteria(b)
	if msg := b.planned[0].msg; !strings.Contains(msg.Description, "- "+want) {
		t.Errorf("task description lacks the gap line with its reason:\n%s", msg.Description)
	}
}
