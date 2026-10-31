package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// R16 G2: a fix round only for a small, actionable problem. Every not_met
// criterion and critical/major defect must carry a concrete reference, and
// there may be at most VERIFIER_FIX_MAX_ITEMS (default 5) of them.
func TestGateFixRoundIsSmallAndActionable(t *testing.T) {
	gap, gapLine := gapCriterion("AC-11", "criterion AC-11 is non-code: satisfied by the PR text; verify manually")
	located := Defect{Severity: SeverityMajor, File: "src/map.js", Line: 12, Category: "logic", Description: "wrong tile URL"}
	unlocated := Defect{Severity: SeverityMajor, Category: "missing_tests", Description: "no test covers the new branch"}
	minorLocated := Defect{Severity: SeverityMinor, File: "a.go", Line: 2, Category: "style", Description: "naming"}
	minorUnlocated := Defect{Severity: SeverityMinor, Category: "style", Description: "naming"}
	nm := func(id, note string, refs ...Ref) CriterionResult {
		if refs == nil {
			refs = []Ref{}
		}
		return CriterionResult{ID: id, Status: StatusNotMet, Evidence: refs, Note: note}
	}

	cases := []struct {
		name      string
		v         Verdict
		want      Action
		reasonHas []string
	}{
		{"not_met with a path:line ref", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "x", "src/a.go:4")}, Overall: OverallRequestChanges}, ActionRequestChanges, []string{"AC-1"}},
		{"not_met with a range ref", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "x", "src/a.go:4-9")}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"not_met naming a file:line in the note", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "index.html:840 still loads the keyless tiles")}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"not_met naming a directory path in the note", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "nothing under docs/setup.txt mentions the key")}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		// Review minor 3: a bare name with an extension is prose, not a file.
		{"not_met mentioning Node.js in prose", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "the feature needs Node.js and is not wired up")}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-1"}},
		{"not_met mentioning index.html without a line", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "index.html still loads the keyless tiles")}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-1"}},
		{"not_met naming a plan command in backticks", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "`npm test` fails on the new case")}, Overall: OverallRequestChanges}.WithPlanChecks([]string{"npm test"}), ActionRequestChanges, nil},
		{"not_met naming a longer form of a plan command", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "`go test ./... -run TestX` fails")}, Overall: OverallRequestChanges}.WithPlanChecks([]string{"go test ./..."}), ActionRequestChanges, nil},
		{"not_met naming a check kind in backticks", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "the `lint` check fails")}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		// Review minor 3: other backticked text is not a check.
		{"not_met with a backticked identifier only", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "`apiKey` is never read")}, Overall: OverallRequestChanges}.WithPlanChecks([]string{"npm test"}), ActionEscalate, []string{"no actionable defect for AC-1"}},
		{"not_met with a backticked command the plan does not run", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "`npm test` fails")}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-1"}},
		{"not_met naming a Go test", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "TestPager_LastPage fails")}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"not_met tied to a located defect naming it", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "the boundary is wrong")}, Defects: []Defect{{Severity: SeverityMajor, File: "p.go", Line: 3, Category: "off_by_one", Description: "AC-1: last page dropped"}}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"not_met explained by an unattributed located defect", Verdict{PerCriterion: []CriterionResult{nm("AC-2", "nil user panics")}, Defects: []Defect{{Severity: SeverityMajor, File: "user.go", Line: 14, Category: "nil_dereference", Description: "u.First on nil"}}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"a located minor defect does not make a not_met actionable", Verdict{PerCriterion: []CriterionResult{nm("AC-2", "incomplete")}, Defects: []Defect{minorLocated}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-2"}},
		{"not_met with only a call id and vague prose", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "the feature does not seem complete", "call_7")}, Overall: OverallRequestChanges}, ActionEscalate, []string{"verifier gave no actionable defect for AC-1"}},
		{"not_met vague, a located defect for another criterion", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "incomplete"), nm("AC-2", "x", "a.go:1")}, Defects: []Defect{{Severity: SeverityMajor, File: "p.go", Line: 3, Category: "logic", Description: "AC-2 wrong"}}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-1"}},
		{"a major defect with no file", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "x", "a.go:1")}, Defects: []Defect{unlocated}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for major defect", "missing_tests"}},
		{"a located major defect", Verdict{Defects: []Defect{located}, Overall: OverallRequestChanges}, ActionRequestChanges, []string{"1 major"}},
		{"an unlocated minor defect does not block a round", Verdict{PerCriterion: []CriterionResult{nm("AC-1", "x", "a.go:1")}, Defects: []Defect{minorUnlocated}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
		{"actionable not_met + recorded gap", Verdict{PerCriterion: []CriterionResult{nm("AC-8", "x", "index.html:840"), gap}, Gaps: []string{gapLine}, Overall: OverallRequestChanges}, ActionRequestChanges, []string{gapLine}},
		{"vague not_met + recorded gap escalates with both", Verdict{PerCriterion: []CriterionResult{nm("AC-8", "not done"), gap}, Gaps: []string{gapLine}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for AC-8", gapLine}},
		{"request_changes word, nothing listed", Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusMet, Evidence: []Ref{"a.go:1"}}}, Overall: OverallRequestChanges}, ActionEscalate, []string{"no actionable defect for its request_changes"}},
		{"request_changes word with a located minor defect", Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusMet, Evidence: []Ref{"a.go:1"}}}, Defects: []Defect{minorLocated}, Overall: OverallRequestChanges}, ActionRequestChanges, nil},
	}
	for _, c := range cases {
		d := Gate(c.v)
		if d.Action != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, d.Action, d.Reason, c.want)
			continue
		}
		for _, s := range c.reasonHas {
			if !strings.Contains(d.Reason, s) {
				t.Errorf("%s: reason %q lacks %q", c.name, d.Reason, s)
			}
		}
	}
}

// Run records the commands the plan runs on the verdict, so a backticked
// plan command in a note is a check the gate accepts.
func TestRunRecordsThePlanCommandsForTheGate(t *testing.T) {
	in := runInput(t)
	in.TaskSpecs = []TaskSpec{{TaskID: "t1", Spec: plan.TaskSpec{Verification: []plan.VerificationStep{{CheckKind: "test", Command: "go test ./..."}}}}}
	if got := in.PlanCommands(); len(got) != 1 || got[0] != "go test ./..." {
		t.Fatalf("PlanCommands = %q", got)
	}
	loop, _ := scripted(`{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"not_met","evidence":[],"note":"` + "`go test ./...`" + ` fails on the empty page"}],"defects":[],"overall":"request_changes"}`)
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, in)
	if v.Error != "" {
		t.Fatalf("answer rejected: %s", v.Error)
	}
	if d := Gate(v); d.Action != ActionRequestChanges {
		t.Errorf("gate = %+v, want request_changes (the note names the plan's command)", d)
	}
	if d := Gate(v.WithPlanChecks(nil)); d.Action != ActionEscalate {
		t.Errorf("without the plan's commands gate = %+v, want escalate", d)
	}
}

func manyItems(notMet, majors int) Verdict {
	v := Verdict{Overall: OverallRequestChanges}
	for i := 1; i <= notMet; i++ {
		v.PerCriterion = append(v.PerCriterion, CriterionResult{ID: fmt.Sprintf("AC-%d", i), Status: StatusNotMet, Evidence: []Ref{Ref(fmt.Sprintf("a.go:%d", i))}, Note: "x"})
	}
	for i := 1; i <= majors; i++ {
		v.Defects = append(v.Defects, Defect{Severity: SeverityMajor, File: "b.go", Line: i, Category: "logic", Description: fmt.Sprintf("d%d", i)})
	}
	return v
}

func TestGateFixRoundSizeBound(t *testing.T) {
	t.Setenv(FixMaxItemsEnv, "")
	if FixMaxItems() != 5 {
		t.Fatalf("default FixMaxItems = %d, want 5", FixMaxItems())
	}
	if d := Gate(manyItems(3, 2)); d.Action != ActionRequestChanges {
		t.Errorf("5 items (at the limit): %+v, want request_changes", d)
	}
	d := Gate(manyItems(4, 2))
	if d.Action != ActionEscalate || !strings.Contains(d.Reason, "too many problems for an automatic fix round") || !strings.Contains(d.Reason, "6 not-met criteria and critical/major defects, limit 5") {
		t.Errorf("6 items: %+v, want escalate (too many)", d)
	}
	// Minor defects do not count.
	v := manyItems(5, 0)
	for i := 0; i < 10; i++ {
		v.Defects = append(v.Defects, Defect{Severity: SeverityMinor, Category: "style", Description: "m"})
	}
	if d := Gate(v); d.Action != ActionRequestChanges {
		t.Errorf("5 items + 10 minors: %+v, want request_changes", d)
	}

	t.Setenv(FixMaxItemsEnv, "2")
	if d := Gate(manyItems(2, 1)); d.Action != ActionEscalate || !strings.Contains(d.Reason, "limit 2") {
		t.Errorf("limit 2, 3 items: %+v, want escalate", d)
	}
	if d := Gate(manyItems(1, 1)); d.Action != ActionRequestChanges {
		t.Errorf("limit 2, 2 items: %+v, want request_changes", d)
	}
	for _, bad := range []string{"0", "-3", "many"} {
		t.Setenv(FixMaxItemsEnv, bad)
		if FixMaxItems() != DefaultFixMaxItems {
			t.Errorf("%s=%q: FixMaxItems = %d, want the default", FixMaxItemsEnv, bad, FixMaxItems())
		}
	}
}

// The seeded-defect evaluation counts what the verifier found, whatever the
// gate does with it: a security defect escalates to a human but is found.
func TestFixtureDetectedIgnoresTheFixRoundBounds(t *testing.T) {
	f := Fixture{Expect: FixtureExpect{CriterionIDs: []string{"AC-1"}, File: "auth.go"}}
	sec := Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusMet, Evidence: []Ref{"auth.go:1"}}},
		Defects: []Defect{{Severity: SeverityCritical, File: "auth.go", Line: 4, Category: "authz", Description: "d"}}, Overall: OverallRequestChanges}
	if Gate(sec).Action != ActionEscalate {
		t.Fatalf("a security defect gates to %s, want escalate", Gate(sec).Action)
	}
	if !f.Detected(sec) {
		t.Error("a security defect in the expected file is a detection")
	}
	vague := Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{}, Note: "missing"}}, Overall: OverallRequestChanges}
	if !f.Detected(vague) {
		t.Error("a not_met on the expected criterion is a detection even without a reference")
	}
	nv := Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusNotMet, Note: "x"}, {ID: "AC-2", Status: StatusNotVerifiable, Note: "could not run"}}, Overall: OverallRequestChanges}
	if f.Detected(nv) {
		t.Error("a verdict with a criterion the verifier could not verify is not a detection")
	}
}
