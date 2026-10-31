package main

// R13: a task's own derived criteria are cited by it (D1), derived criteria
// persist across repair rounds (D2), and a criterion that targets the PR
// text is a non-code gap (D3). Fixtures: the attempt-13 corpus case
// (testdata/corpus/attempt13), item 63f733e0.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// attempt13Brief is the attempt-13 brief's criteria as the handler loads
// them (untrusted marks stripped, validated).
func attempt13Brief(t *testing.T) *plan.BriefCriteria {
	t.Helper()
	structured := loadCorpusBrief(t, filepath.Join(corpusDir, "attempt13"))
	plan.StripNonCode(structured.Criteria)
	brief, err := briefCriteriaOf(structured)
	if err != nil || brief == nil {
		t.Fatalf("attempt-13 brief criteria: %v", err)
	}
	return brief
}

// attempt13Response parses one recorded decomposition response.
func attempt13Response(t *testing.T, file string) []decomposedTask {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, "attempt13", file))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := parseTaskArray(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func violationsOf(vs []plan.Violation, code plan.ViolationCode) []plan.Violation {
	var out []plan.Violation
	for _, v := range vs {
		if v.Code == code {
			out = append(out, v)
		}
	}
	return out
}

// D1: the attempt-13 round-1 shape — task 9 declares derived AC-11 with
// empty criterion_ids — validates with no no_criteria for task 9 and no
// orphan AC-11.
func TestAttempt13_OwnDerivedCriterionIsCited(t *testing.T) {
	raw := attempt13Response(t, "response_1.txt")
	if len(raw) != 9 || len(raw[8].CriterionIDs) != 0 || len(raw[8].DerivedCriteria) != 1 || raw[8].DerivedCriteria[0].ID != "AC-11" {
		t.Fatalf("fixture: want task 9 declaring AC-11 and citing nothing, got %+v", raw[len(raw)-1])
	}
	vs, _, err := validateCaptured(attempt13Brief(t), raw, "", []evidence.CheckKind{"test"}, false)
	if err != nil {
		t.Fatalf("strict pass: %v", err)
	}
	if got := strings.Join(raw[8].CriterionIDs, ","); got != "AC-11" {
		t.Errorf("task 9 criterion_ids = %q, want AC-11", got)
	}
	for _, v := range violationsOf(vs, plan.CodeNoCriteria) {
		if v.TaskIndex == 9 {
			t.Errorf("no_criteria for task 9: %s", v.Message)
		}
	}
	for _, v := range violationsOf(vs, plan.CodeOrphanCriterion) {
		if v.CriterionID == "AC-11" {
			t.Errorf("orphan AC-11: %s", v.Message)
		}
	}
}

func TestCiteOwnDerivedCriteria(t *testing.T) {
	tasks := []decomposedTask{
		{Title: "a", CriterionIDs: []string{"AC-1"}, DerivedCriteria: []derivedCriterion{{ID: "AC-5", Text: "x", Reason: "r", Risk: plan.RiskLow}}},
		{Title: "b", CriterionIDs: []string{"AC-6"}, DerivedCriteria: []derivedCriterion{{ID: "AC-6", Text: "y", Reason: "r", Risk: plan.RiskLow}}},
		{Title: "c", DerivedCriteria: []derivedCriterion{{ID: "eleven", Text: "z", Reason: "r", Risk: plan.RiskLow}}},
	}
	notes := citeOwnDerivedCriteria(tasks)
	if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-1,AC-5" {
		t.Errorf("task a = %q, want AC-1,AC-5", got)
	}
	if got := strings.Join(tasks[1].CriterionIDs, ","); got != "AC-6" {
		t.Errorf("task b = %q, want AC-6 once", got)
	}
	if len(tasks[2].CriterionIDs) != 0 {
		t.Errorf("an invalid derived id is left to the strict pass, got %v", tasks[2].CriterionIDs)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "AC-5") {
		t.Errorf("notes = %v, want one for AC-5", notes)
	}
}

// derivedOf returns the derived criterion id of the plan's tasks, or nil.
func derivedOf(tasks []decomposedTask, id string) *derivedCriterion {
	for i := range tasks {
		for j := range tasks[i].DerivedCriteria {
			if tasks[i].DerivedCriteria[j].ID == id {
				return &tasks[i].DerivedCriteria[j]
			}
		}
	}
	return nil
}

// D2: the attempt-13 repair shape — task 9 cites AC-11 and no task declares
// it — validates: the repair round knows AC-11 from round 1, with its text.
// Round 1 is attempt 13's own first answer with task 1's deliverable
// removed, so that a repair round is asked for (with D1 the recorded first
// answer is accepted as it is).
func TestAttempt13_RepairRoundKnowsEarlierDerivedCriteria(t *testing.T) {
	brief := attempt13Brief(t)
	round1 := attempt13Response(t, "response_1.txt")
	round1[0].Deliverable = ""
	want := round1[8].DerivedCriteria[0]
	round2 := attempt13Response(t, "response_2.txt")
	if derivedOf(round2, "AC-11") != nil || !containsID(round2[8].CriterionIDs, "AC-11") {
		t.Fatalf("fixture: want the repair citing AC-11 without declaring it")
	}
	rounds := [][]decomposedTask{round1, round2}
	calls := 0
	decompose := func(repair string) ([]decomposedTask, error) {
		tasks := rounds[calls]
		calls++
		if calls == 2 && !strings.Contains(repair, "states no deliverable") {
			t.Errorf("the repair prompt does not ask for the deliverable:\n%s", repair)
		}
		citeOwnDerivedCriteria(tasks)
		return tasks, nil
	}
	out := planWithRepair(uuid.Nil, decompose, capturedBuilder(brief))
	if out.rejected() {
		t.Fatalf("plan rejected after %d attempt(s): %v", out.attempts, plan.Errors(out.violations))
	}
	if out.attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (the decomposition and the repair round)", out.attempts)
	}
	for _, v := range violationsOf(out.violations, plan.CodeUnknownCriterion) {
		t.Errorf("unknown criterion: %s", v.Message)
	}
	var ac11 *plan.Criterion
	for i, c := range out.build.criteria.Criteria {
		if c.ID == "AC-11" {
			ac11 = &out.build.criteria.Criteria[i]
		}
	}
	if ac11 == nil || ac11.Text != want.Text || ac11.Source.Kind != plan.SourceDerived || ac11.Source.Reason != want.Reason {
		t.Fatalf("AC-11 after the repair = %+v, want round 1's definition %+v", ac11, want)
	}
	if d := derivedOf(out.build.raw, "AC-11"); d == nil || !containsID(out.build.raw[8].CriterionIDs, "AC-11") {
		t.Errorf("task 9 of the repaired plan: criterion_ids %v, derived %+v", out.build.raw[8].CriterionIDs, out.build.raw[8].DerivedCriteria)
	}
}

// D3: attempt 13's task 9 names no repository file and produces the PR
// text: it is dropped as non-code work, and AC-11 is marked non-code with
// "satisfied by the PR text" — a warning, not an orphan — and recorded on
// every subtask's message (non_code_criteria) for review and pr-coordinator.
func TestAttempt13_PRDescriptionCriterionIsANonCodeGap(t *testing.T) {
	raw := attempt13Response(t, "response_1.txt")
	citeOwnDerivedCriteria(raw)
	b := capturedBuilder(attempt13Brief(t))(raw)
	vs := b.validate()
	if plan.HasErrors(vs) {
		// verifier_missing (AC-3 build, AC-8 test) is the planner's to
		// attach (R9); nothing else may be left.
		for _, v := range plan.Errors(vs) {
			if v.Code != plan.CodeVerifierMissing {
				t.Errorf("error: [%s] %s", v.Code, v.Message)
			}
		}
	}
	var task9 *plan.TaskSpecWithFiles
	for i := range b.specs {
		if b.specs[i].Index == 9 {
			task9 = &b.specs[i]
		}
	}
	if task9 == nil || task9.Dropped != nonImplementationDropReason || !task9.NonCode {
		t.Fatalf("task 9 = %+v, want dropped as non-code work", task9)
	}
	var warned bool
	for _, v := range vs {
		if v.CriterionID != "AC-11" {
			continue
		}
		if v.Code != plan.CodeNonCodeCriterion {
			t.Errorf("AC-11: [%s] %s", v.Code, v.Message)
		}
		warned = v.Message == "criterion AC-11 is non-code: satisfied by the PR text; verify manually"
	}
	if !warned {
		t.Errorf("no non_code_criterion warning with the PR-text gap line for AC-11: %+v", vs)
	}
	attachNonCodeCriteria(b)
	if len(b.planned) == 0 {
		t.Fatal("nothing planned")
	}
	for _, p := range b.planned {
		var found bool
		for _, c := range p.msg.NonCodeCriteria {
			if c.ID == "AC-11" && c.NonCode && c.NonCodeReason == plan.PRTextReason && c.Text == raw[8].DerivedCriteria[0].Text {
				found = true
			}
		}
		if !found {
			t.Errorf("subtask %q: non_code_criteria %+v lack AC-11 with the PR-text reason", p.msg.Title, p.msg.NonCodeCriteria)
		}
	}
}

// A step that produces the PR text is kept when it names a repository
// file (it is then code work, for example a CHANGELOG edit).
func TestPRTextStepWithAFileIsKept(t *testing.T) {
	tasks := []decomposedTask{
		{Title: "Add pull request description", Deliverable: "The PR description states the key"},
		{Title: "Add release notes", Deliverable: "CHANGELOG.md has a release note", Files: []string{"CHANGELOG.md"}},
		{Title: "Add the client-config route", Deliverable: "GET /api/client-config", Files: []string{"src/server/index.ts"}},
	}
	got := filterInvalidTasks(tasks)
	if len(got) != 2 || got[0].Title != "Add release notes" || got[1].Title != "Add the client-config route" {
		t.Errorf("kept = %+v", got)
	}
}

func TestCarriedDerivedCriteria(t *testing.T) {
	ac11 := derivedCriterion{ID: "AC-11", Text: "State in the pull request description that the owner must set TILES_API_KEY", Reason: "deployment", Risk: plan.RiskLow}
	earlier := &planBuild{
		raw:      []decomposedTask{{Title: "pr", CriterionIDs: []string{"AC-11"}, DerivedCriteria: []derivedCriterion{ac11}}},
		criteria: &plan.BriefCriteria{Criteria: []plan.Criterion{{ID: "AC-1"}, {ID: "AC-10"}, ac11.criterion()}},
		folds:    []plan.Fold{{From: "AC-12", Into: "AC-1"}},
	}
	sameDerived := func(a, b derivedCriterion) bool {
		return a.ID == b.ID && a.Text == b.Text && a.Reason == b.Reason && a.Risk == b.Risk
	}

	t.Run("a citation without a definition gets the earlier one", func(t *testing.T) {
		c := newCarriedDerived()
		c.remember(earlier)
		tasks := []decomposedTask{{Title: "a", CriterionIDs: []string{"AC-1"}}, {Title: "b", CriterionIDs: []string{"AC-11"}}, {Title: "c", CriterionIDs: []string{"AC-11"}}}
		notes := c.restore(tasks)
		if len(tasks[1].DerivedCriteria) != 1 || !sameDerived(tasks[1].DerivedCriteria[0], ac11) || len(tasks[2].DerivedCriteria) != 0 || len(tasks[0].DerivedCriteria) != 0 {
			t.Fatalf("restored on the wrong task(s): %+v", tasks)
		}
		if len(notes) != 1 || !strings.Contains(notes[0], "restored its definition") {
			t.Errorf("notes = %v", notes)
		}
	})

	t.Run("an uncited earlier criterion is not restored", func(t *testing.T) {
		c := newCarriedDerived()
		c.remember(earlier)
		tasks := []decomposedTask{{Title: "a", CriterionIDs: []string{"AC-1"}}}
		if notes := c.restore(tasks); len(notes) != 0 || len(tasks[0].DerivedCriteria) != 0 {
			t.Errorf("restored an uncited criterion: %v %+v", notes, tasks)
		}
	})

	t.Run("a redeclaration with the same text is a citation of the earlier one", func(t *testing.T) {
		c := newCarriedDerived()
		c.remember(earlier)
		same := ac11
		same.Text = "State in the pull request description that the owner must set the TILES_API_KEY"
		tasks := []decomposedTask{{Title: "a", DerivedCriteria: []derivedCriterion{same}}}
		c.restore(tasks)
		if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-11" {
			t.Errorf("criterion_ids = %q, want AC-11", got)
		}
		if len(tasks[0].DerivedCriteria) != 1 || !sameDerived(tasks[0].DerivedCriteria[0], ac11) {
			t.Errorf("derived = %+v, want the earlier definition", tasks[0].DerivedCriteria)
		}
	})

	t.Run("a redeclaration with different text is renumbered past every earlier id", func(t *testing.T) {
		c := newCarriedDerived()
		c.remember(earlier)
		other := derivedCriterion{ID: "AC-11", Text: "The server exposes a health endpoint at /healthz", Reason: "ops", Risk: plan.RiskLow}
		tasks := []decomposedTask{{Title: "a", CriterionIDs: []string{"AC-1"}, DerivedCriteria: []derivedCriterion{other}}}
		c.restore(tasks)
		// AC-12 is a folded id of the earlier round: the fresh id skips it.
		if got := strings.Join(tasks[0].CriterionIDs, ","); got != "AC-1,AC-11,AC-13" {
			t.Errorf("criterion_ids = %q, want AC-1,AC-11,AC-13", got)
		}
		if d := derivedOf(tasks, "AC-13"); d == nil || d.Text != other.Text {
			t.Errorf("renumbered = %+v", d)
		}
		if d := derivedOf(tasks, "AC-11"); d == nil || d.Text != ac11.Text {
			t.Errorf("AC-11 = %+v, want the earlier text", d)
		}
	})

	t.Run("the first round restores nothing", func(t *testing.T) {
		tasks := []decomposedTask{{Title: "a", CriterionIDs: []string{"AC-11"}}}
		if notes := newCarriedDerived().restore(tasks); notes != nil || len(tasks[0].DerivedCriteria) != 0 {
			t.Errorf("restored with no earlier round: %v", notes)
		}
	})
}
