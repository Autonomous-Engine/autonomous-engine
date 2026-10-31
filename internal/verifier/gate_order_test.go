package verifier

import (
	"strings"
	"testing"
)

// R16 G1: recorded gaps (non-code, unowned, PR-text criteria; R3/R8/R13)
// no longer pre-empt a fix round. Only a real not_verifiable criterion (the
// model could not verify a code criterion) escalates ahead of not_met; a
// verdict whose only open items are recorded gaps still escalates (R3
// fail-closed: never approve with a recorded gap).

// gapCriterion is a recorded gap as Run appends it: not_verifiable, its note
// the gap line, the line also in Gaps.
func gapCriterion(id, line string) (CriterionResult, string) {
	return CriterionResult{ID: id, Status: StatusNotVerifiable, Evidence: []Ref{}, Note: line}, line
}

func TestGateRecordedGapsDoNotPreemptAFixRound(t *testing.T) {
	met := CriterionResult{ID: "AC-1", Status: StatusMet, Evidence: []Ref{"a.go:1"}}
	notMet := CriterionResult{ID: "AC-2", Status: StatusNotMet, Evidence: []Ref{"a.go:7"}, Note: "boundary is off by one"}
	realNV := CriterionResult{ID: "AC-4", Status: StatusNotVerifiable, Evidence: []Ref{}, Note: "could not run the integration suite"}
	gap, gapLine := gapCriterion("AC-3", "criterion AC-3 is non-code: deployed to staging; verify manually")
	prText, prLine := gapCriterion("AC-11", "criterion AC-11 is non-code: satisfied by the PR text; verify manually")
	unowned, unownedLine := gapCriterion("AC-5", "criterion AC-5 is unowned after planning: the claim is corrected; verify manually")
	major := Defect{Severity: SeverityMajor, File: "a.go", Line: 3, Category: "logic", Description: "wrong result"}
	critical := Defect{Severity: SeverityCritical, File: "a.go", Line: 9, Category: "nil_dereference", Description: "panics on nil"}
	security := Defect{Severity: SeverityMajor, File: "auth.go", Line: 4, Category: "authz", Description: "token not checked"}
	minor := Defect{Severity: SeverityMinor, File: "a.go", Line: 2, Category: "style", Description: "naming"}

	// live-run attempt shape: AC-8 not met (the tile URL is unchanged),
	// AC-11 a PR-text gap.
	ac8 := CriterionResult{ID: "AC-8", Status: StatusNotMet, Evidence: []Ref{"index.html:840"}, Note: "the tile URL still points at the keyless endpoint"}

	cases := []struct {
		name       string
		v          Verdict
		want       Action
		reasonHas  []string
		reasonLack []string
	}{
		{"not_met + recorded gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{met, notMet, gap}, Gaps: []string{gapLine}, Overall: OverallRequestChanges},
			ActionRequestChanges, []string{"criteria not met: AC-2", "recorded gaps: " + gapLine}, nil},
		{"live run: AC-8 not_met + AC-11 PR-text gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{ac8, prText}, Gaps: []string{prLine}, Overall: OverallRequestChanges},
			ActionRequestChanges, []string{"AC-8", prLine}, nil},
		{"not_met + unowned gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{notMet, unowned}, Gaps: []string{unownedLine}, Overall: OverallRequestChanges},
			ActionRequestChanges, []string{"AC-2", unownedLine}, nil},
		{"major defect + recorded gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{met, gap}, Gaps: []string{gapLine}, Defects: []Defect{major}, Overall: OverallApprove},
			ActionRequestChanges, []string{"1 major", gapLine}, nil},
		{"critical defect + recorded gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{met, gap}, Gaps: []string{gapLine}, Defects: []Defect{critical}, Overall: OverallRequestChanges},
			ActionRequestChanges, []string{"1 critical", gapLine}, nil},
		{"only a recorded gap, rest met → escalate (R3 fail-closed)",
			Verdict{PerCriterion: []CriterionResult{met, gap}, Gaps: []string{gapLine}, Defects: []Defect{minor}, Overall: OverallApprove},
			ActionEscalate, []string{"AC-3", gapLine}, nil},
		{"several recorded gaps only → escalate",
			Verdict{PerCriterion: []CriterionResult{met, gap, prText}, Gaps: []string{gapLine, prLine}, Overall: OverallApprove},
			ActionEscalate, []string{"AC-3", "AC-11"}, nil},
		{"real unverifiable beats not_met → escalate naming it",
			Verdict{PerCriterion: []CriterionResult{notMet, realNV}, Overall: OverallRequestChanges},
			ActionEscalate, []string{"could not verify: AC-4"}, nil},
		{"real unverifiable + recorded gap + not_met → escalate naming only the real one",
			Verdict{PerCriterion: []CriterionResult{notMet, realNV, gap}, Gaps: []string{gapLine}, Overall: OverallRequestChanges},
			ActionEscalate, []string{"could not verify: AC-4", gapLine}, []string{"could not verify: AC-4, AC-3"}},
		{"security defect + not_met + gap → escalate",
			Verdict{PerCriterion: []CriterionResult{notMet, gap}, Gaps: []string{gapLine}, Defects: []Defect{security}, Overall: OverallRequestChanges},
			ActionEscalate, []string{"security", "auth.go:4"}, nil},
		{"security defect alone → escalate",
			Verdict{PerCriterion: []CriterionResult{met}, Defects: []Defect{security}, Overall: OverallApprove},
			ActionEscalate, []string{"security"}, nil},
		{"error → escalate",
			Verdict{PerCriterion: []CriterionResult{notMet, gap}, Gaps: []string{gapLine}, Overall: OverallRequestChanges, Error: "tool loop failed"},
			ActionEscalate, []string{"tool loop failed"}, nil},
		{"model escalates with not_met + gap → escalate",
			Verdict{PerCriterion: []CriterionResult{notMet, gap}, Gaps: []string{gapLine}, Overall: OverallEscalate},
			ActionEscalate, []string{"the verifier escalated"}, nil},
		{"model escalates, all met → escalate",
			Verdict{PerCriterion: []CriterionResult{met}, Overall: OverallEscalate},
			ActionEscalate, []string{"the verifier escalated"}, nil},
		{"all met, no gap → approve",
			Verdict{PerCriterion: []CriterionResult{met}, Defects: []Defect{minor}, Overall: OverallApprove},
			ActionApprove, nil, nil},
		{"not_met, no gap → request_changes",
			Verdict{PerCriterion: []CriterionResult{met, notMet}, Overall: OverallApprove},
			ActionRequestChanges, []string{"AC-2"}, []string{"recorded gaps"}},
		// A not_verifiable criterion whose note is a gap line of ANOTHER
		// criterion is not a recorded gap: the gap line must name its own id.
		{"gap line naming another id is a real unverifiable",
			Verdict{PerCriterion: []CriterionResult{notMet, {ID: "AC-9", Status: StatusNotVerifiable, Note: gapLine}}, Gaps: []string{gapLine}, Overall: OverallRequestChanges},
			ActionEscalate, []string{"could not verify: AC-9"}, nil},
		// A not_verifiable whose note is not in Gaps (the model's own words)
		// is real even when it reads like a gap line.
		{"gap-like note not recorded in Gaps is a real unverifiable",
			Verdict{PerCriterion: []CriterionResult{notMet, gap}, Overall: OverallRequestChanges},
			ActionEscalate, []string{"could not verify: AC-3"}, nil},
	}
	for _, c := range cases {
		d := Gate(c.v)
		if d.Action != c.want {
			t.Errorf("%s: action %s (%s), want %s", c.name, d.Action, d.Reason, c.want)
			continue
		}
		for _, s := range c.reasonHas {
			if !strings.Contains(d.Reason, s) {
				t.Errorf("%s: reason %q lacks %q", c.name, d.Reason, s)
			}
		}
		for _, s := range c.reasonLack {
			if strings.Contains(d.Reason, s) {
				t.Errorf("%s: reason %q has %q", c.name, d.Reason, s)
			}
		}
	}
}
