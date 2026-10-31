package verifier

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// R9: a verification step the planner attached (the repository's one
// whole-suite command) is shown to the verifier as the planner's, never as
// a check of the criteria the task cites.
func TestPlannerAttachedStepIsNotCriterionEvidence(t *testing.T) {
	var b strings.Builder
	renderTaskSpec(&b, TaskSpec{TaskID: "t1", Spec: plan.TaskSpec{
		CriterionIDs: []string{"AC-7"},
		Verification: []plan.VerificationStep{
			{CheckKind: evidence.CheckKindLint, Command: "npm run lint"},
			{CheckKind: evidence.CheckKindTest, Command: "npm run test", AttachedBy: plan.AttachedByPlanner},
		},
	}})
	got := b.String()
	if !strings.Contains(got, "- Verification (lint): npm run lint\n") {
		t.Errorf("the model's own step changed:\n%s", got)
	}
	want := "- Verification (test, attached by the planner): npm run test — the repository's whole suite; it shows the kind is run, not that any criterion is met\n"
	if !strings.Contains(got, want) {
		t.Errorf("prompt lacks %q:\n%s", want, got)
	}
}
