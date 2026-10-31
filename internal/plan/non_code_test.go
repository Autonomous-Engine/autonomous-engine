package plan

import (
	"strings"
	"testing"
)

func nonCodeBrief() BriefCriteria {
	return BriefCriteria{SchemaVersion: SchemaVersion, Criteria: []Criterion{
		{ID: "AC-1", Text: "code", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow},
		{ID: "AC-2", Text: "deployed to staging", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow},
	}}
}

func codeTask(index int, ids ...string) TaskSpecWithFiles {
	return TaskSpecWithFiles{Index: index, Title: "code", RepositoryID: "r", Spec: TaskSpec{
		SchemaVersion: SchemaVersion, CriterionIDs: ids, Deliverable: "d",
		Verification: []VerificationStep{{CheckKind: "test", Command: "go test ./..."}},
	}}
}

// A criterion whose only citing step was dropped as non-code work does not
// block: it is a warning, and the criterion is reported non-code.
func TestSoleOwnerDroppedAsNonCodeDoesNotBlock(t *testing.T) {
	deploy := TaskSpecWithFiles{Index: 2, Title: "Deploy to staging", Spec: TaskSpec{CriterionIDs: []string{"AC-2"}},
		Dropped: "not an implementation task", NonCode: true}
	tasks := []TaskSpecWithFiles{codeTask(1, "AC-1"), deploy}
	vs := ValidatePlan(nonCodeBrief(), tasks, DependencyGraph{}, nil)
	if HasErrors(vs) {
		t.Fatalf("a non-code criterion blocked the plan: %+v", vs)
	}
	ws := Warnings(vs)
	if len(ws) != 1 || ws[0].Code != CodeNonCodeCriterion || ws[0].CriterionID != "AC-2" {
		t.Fatalf("warnings = %+v, want one non_code_criterion for AC-2", ws)
	}
	if !strings.HasPrefix(ws[0].Message, "criterion AC-2 is non-code: ") || !strings.HasSuffix(ws[0].Message, "; verify manually") {
		t.Errorf("warning message = %q", ws[0].Message)
	}

	marks := NonCodeCriteria(nonCodeBrief(), tasks)
	if len(marks) != 1 || !strings.Contains(marks["AC-2"], `task 2 "Deploy to staging"`) {
		t.Fatalf("NonCodeCriteria = %v", marks)
	}
	marked := MarkNonCode(nonCodeBrief().Criteria, marks)
	if marked[0].NonCode || !marked[1].NonCode || marked[1].NonCodeReason != marks["AC-2"] {
		t.Fatalf("MarkNonCode = %+v", marked)
	}
	if got := NonCodeGap(marked[1]); got != ws[0].Message {
		t.Errorf("NonCodeGap = %q, want the warning's message %q", got, ws[0].Message)
	}
	// MarkNonCode copies; the brief is not mutated.
	if nonCodeBrief().Criteria[1].NonCode {
		t.Fatal("unreachable")
	}
}

// A criterion also cited by a step dropped for another reason (no test
// runner, no repository) is still an orphan: that step was code work.
func TestOrphanWithACodeDropStillBlocks(t *testing.T) {
	tasks := []TaskSpecWithFiles{
		codeTask(1, "AC-1"),
		{Index: 2, Title: "Deploy", Spec: TaskSpec{CriterionIDs: []string{"AC-2"}}, Dropped: "process", NonCode: true},
		{Index: 3, Title: "Test it", Spec: TaskSpec{CriterionIDs: []string{"AC-2"}}, Dropped: "test-only, and repository r has no test runner"},
	}
	vs := ValidatePlan(nonCodeBrief(), tasks, DependencyGraph{}, nil)
	errs := Errors(vs)
	if len(errs) != 1 || errs[0].Code != CodeOrphanCriterion || errs[0].CriterionID != "AC-2" {
		t.Fatalf("errors = %+v, want the orphan_criterion for AC-2", errs)
	}
	if len(NonCodeCriteria(nonCodeBrief(), tasks)) != 0 {
		t.Fatal("a criterion with a code-work citation was marked non-code")
	}
}

// A criterion no step cites at all is an orphan as before.
func TestUncitedCriterionIsStillAnOrphan(t *testing.T) {
	vs := ValidatePlan(nonCodeBrief(), []TaskSpecWithFiles{codeTask(1, "AC-1")}, DependencyGraph{}, nil)
	errs := Errors(vs)
	if len(errs) != 1 || errs[0].Code != CodeOrphanCriterion {
		t.Fatalf("errors = %+v", errs)
	}
}

// A criterion that arrives already marked non-code (model output, a stored
// brief) is NOT exempt: the validator ignores the flag, and StripNonCode
// clears it on load. Only the plan's dropped steps make a criterion non-code.
func TestPreMarkedNonCodeCriterionIsNotExempt(t *testing.T) {
	brief := nonCodeBrief()
	brief.Criteria[1].NonCode = true
	brief.Criteria[1].NonCodeReason = "deployment is done by operations"
	vs := ValidatePlan(brief, []TaskSpecWithFiles{codeTask(1, "AC-1")}, DependencyGraph{}, nil)
	errs := Errors(vs)
	if len(errs) != 1 || errs[0].Code != CodeOrphanCriterion || errs[0].CriterionID != "AC-2" {
		t.Fatalf("a pre-marked criterion escaped the ownership rule: %+v", vs)
	}
	if len(NonCodeCriteria(brief, []TaskSpecWithFiles{codeTask(1, "AC-1")})) != 0 {
		t.Fatal("NonCodeCriteria trusted the criterion's own flag")
	}
	StripNonCode(&brief)
	if brief.Criteria[1].NonCode || brief.Criteria[1].NonCodeReason != "" {
		t.Fatalf("StripNonCode left %+v", brief.Criteria[1])
	}
	StripNonCode(nil) // no panic
}

// R8: an unowned criterion's gap line says so and quotes it; the mark is
// stripped with the non-code one, and never exempts it from ownership.
func TestUnownedGapAndStrip(t *testing.T) {
	brief := nonCodeBrief()
	c := brief.Criteria[1]
	c.NonCode, c.NonCodeReason, c.Unowned = true, "owned by no task after planning", true
	if got, want := NonCodeGap(c), "criterion AC-2 is unowned after planning: deployed to staging; verify manually"; got != want || UnownedGap(c) != want {
		t.Fatalf("gap = %q, want %q", got, want)
	}
	brief.Criteria[1] = c
	if errs := Errors(ValidatePlan(brief, []TaskSpecWithFiles{codeTask(1, "AC-1")}, DependencyGraph{}, nil)); len(errs) != 1 || errs[0].Code != CodeOrphanCriterion {
		t.Fatalf("an unowned mark exempted the criterion: %+v", errs)
	}
	StripNonCode(&brief)
	if brief.Criteria[1].Unowned || brief.Criteria[1].NonCode {
		t.Fatalf("StripNonCode left %+v", brief.Criteria[1])
	}
}
