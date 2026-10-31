package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
)

// R8 O2: an orphan the planner cannot re-home is recorded as a gap.

const unownedGapLine = "criterion AC-3 is unowned after planning: The notice reads politely; verify manually"

// A criterion that names no path: no block, a recorded gap on every
// subtask's non_code_criteria, and review records it as not_verifiable with
// the gap line (Verdict.Gaps), so the gate escalates to a human.
func TestUnownedCriterionWithNoPathIsARecordedGap(t *testing.T) {
	brief := orphanBrief("The banner renders", "The banner is styled", "The notice reads politely")
	out, _ := planTwice(t, brief, []decomposedTask{
		orphanStep("Render", []string{"AC-1"}, "web/app.js"),
		orphanStep("Style", []string{"AC-2"}, "web/style.css"),
	})
	if out.rejected() {
		t.Fatalf("plan rejected: %+v", out.violations)
	}
	b := out.build
	if len(b.rehomed) != 0 || strings.Join(b.unowned, ",") != "AC-3" {
		t.Fatalf("rehomed=%+v unowned=%v, want AC-3 recorded as a gap", b.rehomed, b.unowned)
	}
	var warned bool
	for _, v := range out.violations {
		if v.CriterionID == "AC-3" {
			if v.Severity != plan.SeverityWarning || v.Code != plan.CodeNonCodeCriterion || v.Message != unownedGapLine {
				t.Errorf("AC-3 violation = %+v, want a non_code_criterion warning with the gap line", v)
			}
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning for AC-3: %+v", out.violations)
	}

	attachNonCodeCriteria(b)
	msg := b.planned[0].msg
	if len(msg.NonCodeCriteria) != 1 || msg.NonCodeCriteria[0].ID != "AC-3" || !msg.NonCodeCriteria[0].NonCode || !msg.NonCodeCriteria[0].Unowned {
		t.Fatalf("non_code_criteria = %+v, want AC-3 marked unowned", msg.NonCodeCriteria)
	}
	if !strings.Contains(msg.Description, "## Unowned and non-code acceptance criteria") || !strings.Contains(msg.Description, "- "+unownedGapLine) {
		t.Errorf("description lacks the gap:\n%s", msg.Description)
	}
	if len(msg.RehomedCriteria) != 0 {
		t.Errorf("rehomed_criteria = %+v, want none", msg.RehomedCriteria)
	}

	// Review: the stored message reaches the verifier's requirements marked,
	// and the verdict records the gap.
	req, err := verifier.LoadRequirements(context.Background(), planTasksQuerier{messages: [][]byte{mustJSON(t, msg)}}, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findCriterion(req.Criteria, "AC-3")
	if !ok || !c.NonCode || !c.Unowned {
		t.Fatalf("verifier requirements AC-3 = %+v (found %t), want it marked unowned", c, ok)
	}
	in := verifier.Build(verifier.Sources{WorkID: uuid.New(), Requirements: req, Unavailable: "no tree in this test"})
	v := verifier.Run(context.Background(), verifier.Runner{}, in)
	var res *verifier.CriterionResult
	for i := range v.PerCriterion {
		if v.PerCriterion[i].ID == "AC-3" {
			res = &v.PerCriterion[i]
		}
	}
	if res == nil || res.Status != verifier.StatusNotVerifiable || res.Note != unownedGapLine {
		t.Fatalf("AC-3 result = %+v, want not_verifiable with the gap line", res)
	}
	if len(v.Gaps) != 1 || v.Gaps[0] != unownedGapLine {
		t.Fatalf("gaps = %q", v.Gaps)
	}
	if d := verifier.Gate(v); d.Action != verifier.ActionEscalate || !strings.Contains(d.Reason, unownedGapLine) {
		t.Fatalf("gate = %+v, want escalate naming the gap", d)
	}
}

// Every named path must be changed by one step; paths split over two steps
// leave the criterion unowned (recorded as a gap, not guessed).
func TestRehomeNeedsOneStepCoveringEveryPath(t *testing.T) {
	brief := orphanBrief("A", "B", "`src/a.ts` and src/b.ts agree on the format")
	out, _ := planTwice(t, brief, []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	})
	if out.rejected() {
		t.Fatalf("rejected: %+v", out.violations)
	}
	if len(out.build.rehomed) != 0 || strings.Join(out.build.unowned, ",") != "AC-3" {
		t.Fatalf("rehomed=%+v unowned=%v, want AC-3 recorded as a gap", out.build.rehomed, out.build.unowned)
	}
	for _, s := range out.build.raw {
		if containsString(s.CriterionIDs, "AC-3") {
			t.Errorf("AC-3 was attached to %q", s.Title)
		}
	}
}

// A plan with one re-homed criterion and one gap records both.
func TestRehomedAndUnownedTogether(t *testing.T) {
	brief := orphanBrief("A", "B", "`src/a.ts` logs the key once", "The team is told", "C", "D")
	out, _ := planTwice(t, brief, []decomposedTask{
		orphanStep("A", []string{"AC-1", "AC-5"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2", "AC-6"}, "src/b.ts"),
	})
	if out.rejected() {
		t.Fatalf("rejected: %+v", out.violations)
	}
	b := out.build
	if len(b.rehomed) != 1 || b.rehomed[0].CriterionID != "AC-3" || b.rehomed[0].TaskIndex != 1 || strings.Join(b.unowned, ",") != "AC-4" {
		t.Fatalf("rehomed=%+v unowned=%v", b.rehomed, b.unowned)
	}
	attachNonCodeCriteria(b)
	msg := b.planned[0].msg
	if len(msg.RehomedCriteria) != 1 || len(msg.NonCodeCriteria) != 1 || msg.NonCodeCriteria[0].ID != "AC-4" {
		t.Fatalf("message: rehomed=%+v non_code=%+v", msg.RehomedCriteria, msg.NonCodeCriteria)
	}
	if c, ok := findCriterion(msg.Criteria, "AC-3"); !ok || c.NonCode {
		t.Fatalf("criteria = %+v, want AC-3 cited, unmarked", msg.Criteria)
	}
}
